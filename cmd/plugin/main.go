package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
  // "io"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/polarity-dev/polarity-ecs-ebs-plugin/internal"
)

type ErrorResponse struct {
	Err string `json:"Err,omitempty"`
}

type MountResponse struct {
	Err        string `json:"Err,omitempty"`
	MountPoint string `json:"Mountpoint"`
}

func main() {

  // logFile, err := os.OpenFile("/logging/polarity-ecs-ebs.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	// if err != nil {
	// 	log.Fatalf("Failed to open log file: %v", err)
	// }
	// defer logFile.Close()

	// // Crea un writer multiplo: stdout + file
	// multiWriter := io.MultiWriter(os.Stdout, logFile)

	// Imposta il logger per scrivere su entrambi
	// log.SetOutput(multiWriter)
  log.SetOutput(os.Stdout)

	log.Println("Setting up docker plugin...")

	sockPath := os.Getenv("SOCK_PATH")
	if sockPath == "" {
		sockPath = "/run/docker/plugins/pl-ebs.sock"
	}

	log.Println("Sock path is " + sockPath)

	log.Println("Starting Docker Plugin...")

	mux := http.NewServeMux()

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Fatalf("Failed to listen on socket: %v", err)
	}
	defer listener.Close()

	log.Println("Retrieving instance metadata...")
	meta, err := internal.GetInstanceMetadata()
	if err != nil {
		log.Fatalf("Failed to get instance metadata: %v", err)
	}

	log.Printf("Instance Metadata: Region=%s, AvailabilityZone=%s, InstanceID=%s", meta.Region, meta.AvailabilityZone, meta.InstanceID)

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{ "status": "ok", "timestamp": "` + time.Now().Format(time.RFC3339) + `" }`))
	})

	mux.HandleFunc("/Plugin.Activate", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Implements": ["VolumeDriver"]}`))
	})

	mux.HandleFunc("/VolumeDriver.Create", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string
		}
		json.NewDecoder(r.Body).Decode(&req)

		log.Printf("Received Create Request: %+v", req)

		if req.Name == "" {
			response := ErrorResponse{Err: "Name cannot be empty or null"}
			json.NewEncoder(w).Encode(response)
			return
		}

		client, err := internal.InitClient(r.Context(), meta.Region)
		if err != nil {
			response := ErrorResponse{Err: fmt.Sprintf("Failed to initialize EC2 client: %v", err)}
			json.NewEncoder(w).Encode(response)
			return
		}

		vol, err := internal.DescribeVolume(r.Context(), client, req.Name)
		if err != nil {
			response := ErrorResponse{Err: fmt.Sprintf("Failed to describe volume: %v", err)}
			json.NewEncoder(w).Encode(response)
			return
		}

		if *vol.AvailabilityZone != meta.AvailabilityZone {
			response := ErrorResponse{Err: fmt.Sprintf("Volume %s is not in the same availability zone as the instance (%s)", req.Name, meta.AvailabilityZone)}
			json.NewEncoder(w).Encode(response)
			return
		}

		mountpoint := filepath.Join("/mnt", req.Name)
		if _, err := os.Stat(mountpoint); err == nil {
			response := ErrorResponse{Err: "Volume already exists"}
			json.NewEncoder(w).Encode(response)
		} else if os.IsNotExist(err) {
			if err := os.MkdirAll(mountpoint, 0755); err != nil {
				response := ErrorResponse{Err: err.Error()}
				json.NewEncoder(w).Encode(response)
			} else {
				response := ErrorResponse{Err: ""}
				json.NewEncoder(w).Encode(response)
			}
		} else {
			response := ErrorResponse{Err: err.Error()}
			json.NewEncoder(w).Encode(response)
		}

	})

	mux.HandleFunc("/VolumeDriver.Mount", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string
		}
		json.NewDecoder(r.Body).Decode(&req)

		log.Printf("Received Mount Request: %+v", req)

		if req.Name == "" {
			response := MountResponse{Err: "Name cannot be empty or null", MountPoint: ""}
			json.NewEncoder(w).Encode(response)
			return
		}

		client, err := internal.InitClient(r.Context(), meta.Region)
		if err != nil {
			response := MountResponse{Err: fmt.Sprintf("Failed to initialize EC2 client: %v", err), MountPoint: ""}
			json.NewEncoder(w).Encode(response)
			return
		}

		vol, err := internal.DescribeVolume(r.Context(), client, req.Name)
		if err != nil {
			response := MountResponse{Err: fmt.Sprintf("Failed to describe volume: %v", err), MountPoint: ""}
			json.NewEncoder(w).Encode(response)
			return
		}

		// attach the volume using aws sdk
		if vol.State == types.VolumeStateInUse && vol.Attachments[0].InstanceId != nil && *vol.Attachments[0].InstanceId != meta.InstanceID {
			log.Printf("Volume %s is in-use by another instance (%s), detaching...", req.Name, *vol.Attachments[0].InstanceId)

			_, err := internal.DetachVolume(r.Context(), client, req.Name, *vol.Attachments[0].InstanceId)
			if err != nil {
				response := MountResponse{Err: fmt.Sprintf("Failed to detach volume: %v", err), MountPoint: ""}
				json.NewEncoder(w).Encode(response)
				return
			}

			log.Printf("Successfully detached volume %s, waiting to be available", req.Name)
			// NOTE: This overrides the previous volume state check
			vol, err = internal.WaitVolume(r.Context(), client, req.Name, types.VolumeStateAvailable)
			if err != nil {
				response := MountResponse{Err: fmt.Sprintf("Failed to wait for volume to be available: %v", err), MountPoint: ""}
				json.NewEncoder(w).Encode(response)
				return
			}
		}

		if vol.State == types.VolumeStateAvailable {
			log.Printf("Volume %s is available, attaching...", req.Name)
			attachRes, err := internal.AttachVolume(r.Context(), client, req.Name, meta.InstanceID)
			if err != nil {
				response := MountResponse{Err: fmt.Sprintf("Failed to attach volume: %v", err), MountPoint: ""}
				json.NewEncoder(w).Encode(response)
				return
			}
			log.Printf("Successfully attached volume %s: %v, waiting to be in-use state", req.Name, attachRes)

			internal.WaitVolume(r.Context(), client, req.Name, types.VolumeStateInUse)
		} else if vol.State != types.VolumeStateInUse {
			log.Printf("Volume %s is in an unhandled state: %s", req.Name, vol.State)
		}

		mountErr := internal.Mount(req.Name)
		if mountErr != nil {
			response := MountResponse{Err: fmt.Sprintf("Failed to mount volume: %v", mountErr), MountPoint: ""}
			json.NewEncoder(w).Encode(response)
			return
		}

		response := MountResponse{Err: "", MountPoint: filepath.Join("/mnt", req.Name)}
		json.NewEncoder(w).Encode(response)
	})

	mux.HandleFunc("/VolumeDriver.Remove", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"Err": "Invalid JSON: %s"}`, err), http.StatusBadRequest)
			return
		}

		if req.Name == "" {
			response := map[string]string{
				"Err": "Name cannot be empty or null",
			}
			json.NewEncoder(w).Encode(response)
			return
		}

		log.Println("Removing volume " + req.Name)

		volumePath := filepath.Join("/mnt", req.Name)

		if err := os.RemoveAll(volumePath); err != nil {
			response := map[string]string{
				"Err": err.Error(),
			}
			json.NewEncoder(w).Encode(response)
		} else {
			response := map[string]string{
				"Err": "",
			}
			json.NewEncoder(w).Encode(response)
		}
	})

	mux.HandleFunc("/VolumeDriver.Capabilities", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)

		log.Printf("Received Capabilities Request: %+v", req)

		response := map[string]interface{}{
			"Capabilities": map[string]string{
				"Scope": "local",
			},
		}
		json.NewEncoder(w).Encode(response)
	})

	mux.HandleFunc("/VolumeDriver.Get", func(w http.ResponseWriter, r *http.Request) {
		log.Println("\n\nGetting volume")

		var req struct {
			Name string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"Err": "Invalid JSON: %s"}`, err), http.StatusBadRequest)
			return
		}

		if req.Name == "" {
			response := map[string]interface{}{
				"Volume": map[string]interface{}{},
				"Err":    "Name cannot be empty or null",
			}
			json.NewEncoder(w).Encode(response)
			return
		}

		mountpoint := filepath.Join("/mnt", req.Name)
		if _, err := os.Stat(mountpoint); os.IsNotExist(err) {
			response := map[string]interface{}{
				"Volume": map[string]interface{}{},
				"Err":    "Volume not found",
			}
			json.NewEncoder(w).Encode(response)
		} else if err != nil {
			response := map[string]interface{}{
				"Volume": map[string]interface{}{},
				"Err":    err.Error(),
			}
			json.NewEncoder(w).Encode(response)
		} else {
			response := map[string]interface{}{
				"Volume": map[string]interface{}{
					"Name":       req.Name,
					"Mountpoint": mountpoint,
					"Status":     map[string]interface{}{},
				},
				"Err": "",
			}
			json.NewEncoder(w).Encode(response)
		}
	})

	mux.HandleFunc("/VolumeDriver.Unmount", func(w http.ResponseWriter, r *http.Request) {
		log.Println("\n\nUnmounting volume")

		var req struct {
			Name string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"Err": "Invalid JSON: %s"}`, err), http.StatusBadRequest)
			return
		}

		if req.Name == "" {
			response := map[string]string{
				"Err": "Name cannot be empty or null",
			}
			json.NewEncoder(w).Encode(response)
			return
		}

		cmd := exec.Command("/bin/umount", filepath.Join("/mnt", req.Name))
		if err := cmd.Run(); err != nil {
			response := map[string]string{
				"Err": err.Error(),
			}
			json.NewEncoder(w).Encode(response)
			return
		}

		response := map[string]string{
			"Err": "",
		}
		json.NewEncoder(w).Encode(response)
	})

	mux.HandleFunc("/VolumeDriver.Path", func(w http.ResponseWriter, r *http.Request) {
		log.Println("\n\nGetting volume path")

		var req struct {
			Name string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"Err": "Invalid JSON: %s"}`, err), http.StatusBadRequest)
			return
		}

		if req.Name == "" {
			response := map[string]string{
				"Mountpoint": "",
				"Err":        "Name cannot be empty or null",
			}
			json.NewEncoder(w).Encode(response)
			return
		}

		mountpoint := filepath.Join("/mnt", req.Name)
		if _, err := os.Stat(mountpoint); os.IsNotExist(err) {
			response := map[string]string{
				"Mountpoint": "",
				"Err":        "Volume not found",
			}
			json.NewEncoder(w).Encode(response)
		} else if err != nil {
			response := map[string]string{
				"Mountpoint": "",
				"Err":        err.Error(),
			}
			json.NewEncoder(w).Encode(response)
		} else {
			response := map[string]string{
				"Mountpoint": mountpoint,
				"Err":        "",
			}
			json.NewEncoder(w).Encode(response)
		}
	})

	mux.HandleFunc("/VolumeDriver.List", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)

		log.Printf("Received List Volumes Request: %+v", req)

		// os read /mnt dir
		files, err := os.ReadDir("/mnt")
		if err != nil {
			log.Printf("Error reading /mnt directory: %v", err)

			response := map[string]interface{}{
				"Volumes": []string{},
				"Err":     err.Error(),
			}

			json.NewEncoder(w).Encode(response)
		} else {
			response := map[string]interface{}{
				"Volumes": []map[string]string{},
				"Err":     "",
			}

			for _, file := range files {
				if file.IsDir() {
					volume := map[string]string{
						"Name":       file.Name(),
						"Mountpoint": "/mnt/" + file.Name(),
					}
					response["Volumes"] = append(response["Volumes"].([]map[string]string), volume)
				} else {
					log.Printf("Skipping non-directory file: %s", file.Name())
				}
			}
			json.NewEncoder(w).Encode(response)
		}
	})

	log.Println("Plugin HTTP server is starting on", sockPath)
	if err := http.Serve(listener, mux); err != nil {
		log.Fatalf("Failed to serve plugin API: %v", err)
	}
}

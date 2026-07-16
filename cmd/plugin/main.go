package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

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

var Debug string = "false"
var CommitHash string = "unknown"

// Gates the last-resort "terminate the holder" fallback. Off: automating instance
// termination from a volume driver is too broad a blast radius, so a human does it.
var canTerminateInstances = false

func main() {
	if Debug == "true" {
		logFile, err := os.OpenFile("/logging/polarity-ecs-ebs.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("Failed to open log file: %v", err)
		}
		defer logFile.Close()

		multiWriter := io.MultiWriter(os.Stdout, logFile)

		log.SetOutput(multiWriter)
	} else {
		log.SetOutput(os.Stdout)
	}
	log.Println("Starting Polarity EBS Plugin (debug=" + Debug + ", commit=" + CommitHash + ")...")

	sockPath := os.Getenv("SOCK_PATH")
	if sockPath == "" {
		sockPath = "/run/docker/plugins/pl-ebs.sock"
	}

	log.Println("Sock path is " + sockPath)

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
		w.Write([]byte(`{ "status": "ok", "timestamp": "` + time.Now().Format(time.RFC3339) + `", "commit": "` + CommitHash + `" }`))
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

		checkResult := internal.CheckForTasksWithVolumeInUse(req.Name, meta.Region, meta.AvailabilityZone)
		if checkResult.Status == internal.ProcessingError {
			response := MountResponse{Err: fmt.Sprintf("Error checking volume usage: %v", checkResult.Err), MountPoint: ""}
			json.NewEncoder(w).Encode(response)
			return
		}

		// Stop every other task whose container may still be live on the volume before
		// mounting it here.
		forceDetachNeeded := false
		stopNotPermitted := false
		candidates := internal.StopCandidates(checkResult.Tasks)
		if len(candidates) > 0 {
			log.Printf("Volume %s: %d live task(s) using it, stopping them before takeover", req.Name, len(candidates))

			for _, task := range candidates {
				log.Printf("Stopping conflicting task %s (instance: %s, last: %s)", task.TaskArn, task.InstanceID, task.Status)
				stopErr := internal.StopTask(meta.Region, task.ClusterArn, task.TaskArn)
				if stopErr == nil {
					continue
				}
				log.Printf("StopTask failed for %s: %v", task.TaskArn, stopErr)

				// No ecs:StopTask permission: stay on the legacy non-force path and never
				// force-detach a possibly-live volume (same as the old plugin).
				if internal.IsAuthorizationError(stopErr) {
					log.Printf("StopTask not permitted (grant ecs:StopTask to enable takeover) — staying on non-force detach")
					stopNotPermitted = true
				}

				// Can't stop a live task on our own instance: unsafe to take over.
				if task.InstanceID == meta.InstanceID {
					response := MountResponse{Err: fmt.Sprintf("Cannot mount: local task %s (last=%s) could not be stopped: %v", task.TaskArn, task.Status, stopErr), MountPoint: ""}
					json.NewEncoder(w).Encode(response)
					return
				}

				// Stop failed on another, unreachable instance → force detach, but only
				// if we actually have the permission to stop (otherwise stay legacy).
				if !stopNotPermitted {
					forceDetachNeeded = true
				}
			}

			vol, err = internal.DescribeVolume(r.Context(), client, req.Name)
			if err != nil {
				response := MountResponse{Err: fmt.Sprintf("Failed to describe volume after stop: %v", err), MountPoint: ""}
				json.NewEncoder(w).Encode(response)
				return
			}
		}

		// Detach the volume from whichever instance still holds it: non-force if the
		// holder was stopped cleanly, force if it's unreachable, terminate as last resort.
		if vol.State == types.VolumeStateInUse && len(vol.Attachments) > 0 && vol.Attachments[0].InstanceId != nil && *vol.Attachments[0].InstanceId != meta.InstanceID {
			attachedInstance := *vol.Attachments[0].InstanceId
			detached := false

			if !forceDetachNeeded {
				log.Printf("Volume %s in-use by %s, attempting non-force detach...", req.Name, attachedInstance)
				if _, derr := internal.DetachVolume(r.Context(), client, req.Name, attachedInstance); derr != nil {
					log.Printf("Non-force detach call failed: %v", derr)
				} else if _, werr := internal.WaitVolumeTimeout(r.Context(), client, req.Name, types.VolumeStateAvailable, 30*time.Second); werr != nil {
					log.Printf("Volume not available after non-force detach: %v — escalating to force", werr)
				} else {
					detached = true
				}
			}

			// Without ecs:StopTask we stay on the legacy path: never force-detach a
			// possibly-live volume. Fail here; ECS retries until the old task is gone.
			if !detached && stopNotPermitted {
				response := MountResponse{Err: fmt.Sprintf("Volume %s not released by non-force detach; force-detach disabled without ecs:StopTask", req.Name), MountPoint: ""}
				json.NewEncoder(w).Encode(response)
				return
			}

			if !detached {
				log.Printf("Force-detaching volume %s from %s...", req.Name, attachedInstance)
				if _, derr := internal.DetachVolumeWithForce(r.Context(), client, req.Name, attachedInstance, true); derr != nil {
					log.Printf("Force detach call failed: %v", derr)
				}
				if _, werr := internal.WaitVolumeTimeout(r.Context(), client, req.Name, types.VolumeStateAvailable, 60*time.Second); werr != nil {
					// Force-detach didn't free the volume. Terminating the holder is a last
					// resort gated behind canTerminateInstances (off: too broad a blast radius
					// to automate); otherwise fail and let ECS retry / a human step in.
					if !canTerminateInstances {
						response := MountResponse{Err: fmt.Sprintf("Volume %s still not available after force detach: %v (manual intervention required on %s)", req.Name, werr, attachedInstance), MountPoint: ""}
						json.NewEncoder(w).Encode(response)
						return
					}
					log.Printf("Volume still not available after force detach: %v — terminating instance %s", werr, attachedInstance)
					if termErr := internal.TerminateInstance(meta.Region, attachedInstance); termErr != nil {
						response := MountResponse{Err: fmt.Sprintf("All recovery attempts failed: force-detach err=%v, terminate err=%v", werr, termErr), MountPoint: ""}
						json.NewEncoder(w).Encode(response)
						return
					}
					if _, werr2 := internal.WaitVolume(r.Context(), client, req.Name, types.VolumeStateAvailable); werr2 != nil {
						response := MountResponse{Err: fmt.Sprintf("Volume not available after terminating instance %s: %v", attachedInstance, werr2), MountPoint: ""}
						json.NewEncoder(w).Encode(response)
						return
					}
				}
			}

			vol, err = internal.DescribeVolume(r.Context(), client, req.Name)
			if err != nil {
				response := MountResponse{Err: fmt.Sprintf("Failed to describe volume after detach: %v", err), MountPoint: ""}
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

		log.Printf("Received Remove Request: %+v", req)

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
		var req struct {
			Name string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"Err": "Invalid JSON: %s"}`, err), http.StatusBadRequest)
			return
		}

		log.Printf("Received Get Request: %+v", req)

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
		var req struct {
			Name string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"Err": "Invalid JSON: %s"}`, err), http.StatusBadRequest)
			return
		}

		log.Printf("Received Unmount Request: %+v", req)

		if req.Name == "" {
			response := map[string]string{
				"Err": "Name cannot be empty or null",
			}
			json.NewEncoder(w).Encode(response)
			return
		}

		cmd := exec.Command("umount", filepath.Join("/mnt", req.Name))
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
		var req struct {
			Name string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"Err": "Invalid JSON: %s"}`, err), http.StatusBadRequest)
			return
		}

		log.Printf("Received Path Request: %+v", req)

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

	log.Println("Plugin HTTP SOCK server is starting on", sockPath)
	if err := http.Serve(listener, mux); err != nil {
		log.Fatalf("Failed to serve plugin API: %v", err)
	}
}

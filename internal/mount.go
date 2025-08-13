package internal

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type nvmeDevice struct {
	SerialNumber string `json:"SerialNumber"`
	DevicePath   string `json:"DevicePath"`
}

type nvmeList struct {
	Devices []nvmeDevice `json:"Devices"`
}

func runCommand(cmdStr string, args ...string) (string, error) {
	cmd := exec.Command(cmdStr, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func FindDeviceByVolumeID(volumeID string) (string, error) {
	log.Println("Finding device for volumeID:", volumeID)
	dataVolumeID := volumeID[4:]
	sysBlockPath := "/sys/block"

	for attempt := 1; attempt <= 5; attempt++ {
		entries, err := os.ReadDir(sysBlockPath)
		if err != nil {
			return "", fmt.Errorf("failed to read %s: %v", sysBlockPath, err)
		}

		log.Printf("Attempt %d to find device for volumeID %s", attempt, dataVolumeID)

		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "nvme") {
				continue
			}

			serialPath := filepath.Join(sysBlockPath, name, "device", "serial")
			serialBytes, err := os.ReadFile(serialPath)
			if err != nil {
				continue
			}

			serial := (string(serialBytes))
			if strings.Contains(serial, dataVolumeID) {
				return strings.TrimPrefix(name, "/dev/"), nil
			}
		}

		time.Sleep(time.Duration(attempt) * time.Second)
	}

	return "", fmt.Errorf("device with volumeID %s not found after 5 attempts", volumeID)
}

func GetFilesystem(device string) (string, error) {
  if !strings.HasPrefix(device, "/dev") {
    device = "/dev/" + device
  }
	output, err := runCommand("blkid", device)
	if err != nil {
		return "", fmt.Errorf("failed to run blkid on %s: %v", device, err)
	}

	fsType := ""
	for part := range strings.FieldsSeq(output) {
		if strings.HasPrefix(part, "TYPE=") {
			fsType = strings.Trim(part[len("TYPE="):], `"`)
			break
		}
	}

	log.Printf("Device %s has filesystem type: %s", device, fsType)
	return fsType, nil
}

func getMountpoint(device string) (string, error) {
	output, err := runCommand("lsblk", "-f")
	if err != nil {
		return "", fmt.Errorf("failed to run lsblk: %v", err)
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[0] == device {
			return fields[5], nil
		}
	}

	return "", nil
}

func Mount(volumeID string) error {
	device, err := FindDeviceByVolumeID(volumeID)
	if err != nil {
		return fmt.Errorf("error finding device: %v", err)
	}

	filesystem, err := GetFilesystem(device)
	if err != nil {
		return fmt.Errorf("error getting filesystem: %v", err)
	}

	mountpointPath := fmt.Sprintf("/mnt/%s", volumeID)

	if filesystem == "" {
		if _, err := runCommand("mkfs.xfs", "/dev/"+device); err != nil {
			return fmt.Errorf("error creating filesystem: %v", err)
		}

		filesystem = "xfs"

		os.MkdirAll(mountpointPath, 0755)
		if _, err := runCommand("mount", "-t", filesystem, "/dev/"+device, mountpointPath); err != nil {
			return fmt.Errorf("error mounting device: %v", err)
		}

		// Clear directory contents
		if err := os.RemoveAll(mountpointPath + "/*"); err != nil {
			return fmt.Errorf("error clearing mount directory: %v", err)
		}
	}

	mountpoint, err := getMountpoint(device)
	if err != nil {
		return fmt.Errorf("error getting mountpoint: %v", err)
	}

	if mountpoint == "" {
		os.MkdirAll(mountpointPath, 0755)
		if _, err := runCommand("mount", "-t", filesystem, "/dev/"+device, mountpointPath); err != nil {
			return fmt.Errorf("error mounting device: %v", err)
		}
	} else if mountpoint != mountpointPath {
		if _, err := runCommand("umount", mountpoint); err != nil {
			return fmt.Errorf("error unmounting device: %v", err)
		}
		if _, err := runCommand("mount", "-t", filesystem, "/dev/"+device, mountpointPath); err != nil {
			return fmt.Errorf("error remounting device: %v", err)
		}
	}

	return nil
}

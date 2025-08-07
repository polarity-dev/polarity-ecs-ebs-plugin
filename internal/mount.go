package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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

func findDeviceByVolumeID(volumeID string) (string, error) {
	dataVolumeID := volumeID[4:]
	for {
		output, err := runCommand("nvme", "list", "-o", "json")
		if err != nil {
			return "", fmt.Errorf("failed to run nvme list: %v", err)
		}

		var nvmeList nvmeList
		if err := json.Unmarshal([]byte(output), &nvmeList); err != nil {
			return "", fmt.Errorf("failed to parse nvme list json: %v", err)
		}

		for _, device := range nvmeList.Devices {
			if strings.Contains(device.SerialNumber, dataVolumeID) {
				return strings.TrimPrefix(device.DevicePath, "/dev/"), nil
			}
		}

		time.Sleep(1 * time.Second)
	}
}

func getFilesystem(device string) (string, error) {
	output, err := runCommand("lsblk", "-f")
	if err != nil {
		return "", fmt.Errorf("failed to run lsblk: %v", err)
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == device {
			return fields[1], nil
		}
	}

	return "", nil
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
	device, err := findDeviceByVolumeID(volumeID)
	if err != nil {
		return fmt.Errorf("error finding device: %v", err)
	}

	filesystem, err := getFilesystem(device)
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

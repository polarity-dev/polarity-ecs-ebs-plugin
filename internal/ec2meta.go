package internal

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type InstanceMetadata struct {
	Region           string
	AvailabilityZone string
	InstanceID       string
}

func GetInstanceMetadata() (*InstanceMetadata, error) {
	meta := &InstanceMetadata{
		Region:           strings.TrimSpace(os.Getenv("REGION")),
		AvailabilityZone: strings.TrimSpace(os.Getenv("AVAILABILITY_ZONE")),
		InstanceID:       strings.TrimSpace(os.Getenv("INSTANCE_ID")),
	}

	// Se tutte sono settate, ritorna subito
	if meta.Region != "" && meta.AvailabilityZone != "" && meta.InstanceID != "" {
		return meta, nil
	}

	// Altrimenti, prendi il token
	token, err := getIMDSToken()
	if err != nil {
		return nil, fmt.Errorf("failed to get IMDS token: %w", err)
	}

	if meta.Region == "" {
		meta.Region, err = getMetadata(token, "placement/region")
		if err != nil {
			return nil, fmt.Errorf("failed to get region: %w", err)
		}
	}

	if meta.AvailabilityZone == "" {
		meta.AvailabilityZone, err = getMetadata(token, "placement/availability-zone")
		if err != nil {
			return nil, fmt.Errorf("failed to get availability zone: %w", err)
		}
	}

	if meta.InstanceID == "" {
		meta.InstanceID, err = getMetadata(token, "instance-id")
		if err != nil {
			return nil, fmt.Errorf("failed to get instance ID: %w", err)
		}
	}

	return meta, nil
}

func getIMDSToken() (string, error) {
	req, err := http.NewRequest("PUT", "http://169.254.169.254/latest/api/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "3600")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func getMetadata(token, path string) (string, error) {
	url := "http://169.254.169.254/latest/meta-data/" + path
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token", token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

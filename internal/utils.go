package internal

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

var deviceOptions = []string{
	"/dev/sdb", "/dev/sdc", "/dev/sdd", "/dev/sde", "/dev/sdf",
	"/dev/sdg", "/dev/sdh", "/dev/sdi", "/dev/sdj",
}

// findAvailableDevice finds the first available device name to attach a volume.
func findAvailableDevice(ctx context.Context, client *ec2.Client, instanceID string) (string, error) {

	commandEc2 := &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}
	responseEc2, err := client.DescribeInstances(ctx, commandEc2)
	if err != nil {
		return "", fmt.Errorf("failed to describe instances: %w", err)
	}

	if len(responseEc2.Reservations) == 0 || len(responseEc2.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("instance not found with ID: %s", instanceID)
	}

	instance := responseEc2.Reservations[0].Instances[0]
	usedDevices := make(map[string]bool)
	for _, mapping := range instance.BlockDeviceMappings {
		usedDevices[aws.ToString(mapping.DeviceName)] = true
	}

	for _, device := range deviceOptions {
		if !usedDevices[device] {
			return device, nil
		}
	}

	return "", fmt.Errorf("no available device name found")
}

// attachVolume attaches a volume to the EC2 instance.
func AttachVolume(ctx context.Context, client *ec2.Client, volumeID string, instanceID string) (*ec2.AttachVolumeOutput, error) {
	device, err := findAvailableDevice(ctx, client, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to find available device: %w", err)
	}

	commandAttach := &ec2.AttachVolumeInput{
		Device:     aws.String(device),
		InstanceId: aws.String(instanceID),
		VolumeId:   aws.String(volumeID),
	}
	ebs, err := client.AttachVolume(ctx, commandAttach)
	if err != nil {
		return nil, fmt.Errorf("failed to attach volume: %w", err)
	}

	return ebs, nil
}

// detachVolume detaches a volume from the EC2 instance.
func DetachVolume(ctx context.Context, client *ec2.Client, volumeID, instanceID string) (*ec2.DetachVolumeOutput, error) {
	commandDetach := &ec2.DetachVolumeInput{
		InstanceId: aws.String(instanceID),
		VolumeId:   aws.String(volumeID),
	}
	ebs, err := client.DetachVolume(ctx, commandDetach)
	if err != nil {
		return nil, fmt.Errorf("failed to detach volume: %w", err)
	}

	return ebs, nil
}

// waitVolume waits for a volume to reach a specific state.
func WaitVolume(ctx context.Context, client *ec2.Client, volumeID string, state types.VolumeState) (*types.Volume, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			volume, err := DescribeVolume(ctx, client, volumeID)
			if err != nil {
				return nil, fmt.Errorf("failed to describe volume while waiting: %w", err)
			}
			if volume.State == state {
				return volume, nil
			}
			log.Printf("Volume %s is in state %s, waiting for %s...", volumeID, volume.State, state)
		}
	}
}

// describeVolume describes a single volume.
func DescribeVolume(ctx context.Context, client *ec2.Client, volumeID string) (*types.Volume, error) {
	command := &ec2.DescribeVolumesInput{
		VolumeIds: []string{volumeID},
	}
	response, err := client.DescribeVolumes(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("failed to describe volume: %w", err)
	}

	if len(response.Volumes) == 0 {
		return nil, fmt.Errorf("volume not found with ID: %s", volumeID)
	}

	return &response.Volumes[0], nil
}

// initClient loads AWS configuration and creates a new EC2 client.
func InitClient(ctx context.Context, region string) (*ec2.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("unable to load SDK config: %w", err)
	}
	return ec2.NewFromConfig(cfg), nil
}

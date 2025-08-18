package internal

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
)

type Status int

const (
	OK Status = iota
	ProcessingError
	VolumeInUseError
)

func getCurrentAZ() (string, error) {
	// get AZ from env first, to test locally
	az := strings.TrimSpace(os.Getenv("AVAILABILITY_ZONE"))
	if az != "" {
		return az, nil
	}

	// If not set, try to get it from IMDS
	resp, err := http.Get("http://169.254.169.254/latest/meta-data/placement/availability-zone")
	if err != nil {
		return "", fmt.Errorf("impossibile contattare IMDS: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Check single cluster
func checkCluster(ctx context.Context, cfg aws.Config, clusterArn, targetAZ string, volumeFound *bool, mu *sync.Mutex, wg *sync.WaitGroup, volumeToCheck string) {
	defer wg.Done()

	ecsClient := ecs.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	clusterName := *aws.String(clusterArn[strings.LastIndex(clusterArn, "/")+1:])

	// 1. List all container instances in cluster
	ciPaginator := ecs.NewListContainerInstancesPaginator(ecsClient, &ecs.ListContainerInstancesInput{Cluster: &clusterName})
	var ciArns []string
	for ciPaginator.HasMorePages() {
		output, err := ciPaginator.NextPage(ctx)
		if err != nil { log.Printf("error listing instances for %s: %v", clusterName, err); return }
		ciArns = append(ciArns, output.ContainerInstanceArns...)
	}
	if len(ciArns) == 0 { return }

	// 2. Describe container instances to get EC2 IDsq
	describedCIs, err := ecsClient.DescribeContainerInstances(ctx, &ecs.DescribeContainerInstancesInput{Cluster: &clusterName, ContainerInstances: ciArns})
	if err != nil { log.Printf("error describing instances for %s: %v", clusterName, err); return }

	ec2IdToCiArn := make(map[string]string)
	var ec2Ids []string
	for _, ci := range describedCIs.ContainerInstances {
		ec2IdToCiArn[*ci.Ec2InstanceId] = *ci.ContainerInstanceArn
		ec2Ids = append(ec2Ids, *ci.Ec2InstanceId)
	}

	// 3. Describe EC2 instances to filter by AZ
	describedEc2s, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ec2Ids})
	if err != nil { log.Printf("error describing EC2 for %s: %v", clusterName, err); return }

	ciArnsInAZ := make(map[string]bool)
	for _, res := range describedEc2s.Reservations {
		for _, inst := range res.Instances {
			if *inst.Placement.AvailabilityZone == targetAZ {
				ciArnsInAZ[ec2IdToCiArn[*inst.InstanceId]] = true
			}
		}
	}
	if len(ciArnsInAZ) == 0 { return }

	// 4. List and inspect tasks only on instances in the correct AZ
	taskPaginator := ecs.NewListTasksPaginator(ecsClient, &ecs.ListTasksInput{Cluster: &clusterName})
	var taskArns []string
	for taskPaginator.HasMorePages() {
		tasksOutput, err := taskPaginator.NextPage(ctx)
		if err != nil { log.Printf("error listing tasks for %s: %v", clusterName, err); return }
		taskArns = append(taskArns, tasksOutput.TaskArns...)
	}

	if len(taskArns) == 0 { return }

	describedTasks, err := ecsClient.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: &clusterName, Tasks: taskArns})
	if err != nil { log.Printf("error describing tasks for %s: %v", clusterName, err); return }

	taskDefsToInspect := make(map[string]bool)
	for _, task := range describedTasks.Tasks {
		if _, ok := ciArnsInAZ[*task.ContainerInstanceArn]; ok {
			taskDefsToInspect[*task.TaskDefinitionArn] = true
		}
	}

	for taskDefArn := range taskDefsToInspect {
		mu.Lock()
		if *volumeFound { mu.Unlock(); return }
		mu.Unlock()

		defOutput, err := ecsClient.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: &taskDefArn})
		if err != nil { log.Printf("error describing task def %s: %v", taskDefArn, err); continue }

		for _, vol := range defOutput.TaskDefinition.Volumes {
			if *vol.Name == volumeToCheck {
				log.Printf("BLOCKED: Found volume '%s' in use by task def %s", volumeToCheck, taskDefArn)
				mu.Lock()
				*volumeFound = true
				mu.Unlock()
				return
			}
		}
	}
}


func CheckForTasksWithVolumeInUse(volumeToCheck string) (Status, error) {
	log.Println("Starting check for tasks using volume: ", volumeToCheck)
	targetAZ, err := getCurrentAZ()
	if err != nil {
		return ProcessingError, fmt.Errorf("error while retrieving AZ: %v", err)
	}
	log.Printf("📍 AZ is: %s", targetAZ)

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return ProcessingError, fmt.Errorf("error while creating AWS configuration: %v", err)
	}

	ecsClient := ecs.NewFromConfig(cfg)

	log.Println("List all clusters in AZ...")
	clustersOutput, err := ecsClient.ListClusters(ctx, &ecs.ListClustersInput{})
	if err != nil {
		return ProcessingError, fmt.Errorf("cannot list clusters: %v", err)
	}

	if len(clustersOutput.ClusterArns) == 0 {
		log.Println("No clusters found in this AZ.")
		return OK, nil
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	volumeFound := false

	// Check each cluster concurrently
	for _, clusterArn := range clustersOutput.ClusterArns {
		wg.Add(1)
		go checkCluster(ctx, cfg, clusterArn, targetAZ, &volumeFound, &mu, &wg, volumeToCheck)
	}

	wg.Wait() // Wait for all checks to finish

	if volumeFound {
		return VolumeInUseError, fmt.Errorf("volume '%s' is currently in use by task", volumeToCheck)
	}

	return OK, nil
}

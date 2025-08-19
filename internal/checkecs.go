package internal

import (
	"context"
	"fmt"
	"log"
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

// Check single cluster
func checkCluster(ctx context.Context, cfg aws.Config, clusterArn, targetAZ string, volumeFound *int, mu *sync.Mutex, wg *sync.WaitGroup, volumeToCheck string) {
	defer wg.Done()

	ecsClient := ecs.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	clusterName := *aws.String(clusterArn[strings.LastIndex(clusterArn, "/")+1:])

	// 1. List all container instances in cluster
	log.Printf("Checking cluster %s for volume %s in AZ %s", clusterName, volumeToCheck, targetAZ)
	ciPaginator := ecs.NewListContainerInstancesPaginator(ecsClient, &ecs.ListContainerInstancesInput{Cluster: &clusterName})
	var ciArns []string
	for ciPaginator.HasMorePages() {
		output, err := ciPaginator.NextPage(ctx)
		if err != nil {
			log.Printf("error listing instances for %s: %v", clusterName, err)
			return
		}
		ciArns = append(ciArns, output.ContainerInstanceArns...)
	}
	if len(ciArns) == 0 {
		return
	}

	// 2. Describe container instances to get EC2 IDsq
	describedCIs, err := ecsClient.DescribeContainerInstances(ctx, &ecs.DescribeContainerInstancesInput{Cluster: &clusterName, ContainerInstances: ciArns})
	if err != nil {
		log.Printf("error describing instances for %s: %v", clusterName, err)
		return
	}

	ec2IdToCiArn := make(map[string]string)
	var ec2Ids []string
	for _, ci := range describedCIs.ContainerInstances {
		ec2IdToCiArn[*ci.Ec2InstanceId] = *ci.ContainerInstanceArn
		ec2Ids = append(ec2Ids, *ci.Ec2InstanceId)
	}

	// 3. Describe EC2 instances to filter by AZ
	describedEc2s, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ec2Ids})
	if err != nil {
		log.Printf("error describing EC2 for %s: %v", clusterName, err)
		return
	}

	ciArnsInAZ := make(map[string]bool)
	for _, res := range describedEc2s.Reservations {
		for _, inst := range res.Instances {
			if *inst.Placement.AvailabilityZone == targetAZ {
				ciArnsInAZ[ec2IdToCiArn[*inst.InstanceId]] = true
			}
		}
	}
	if len(ciArnsInAZ) == 0 {
		return
	}

	// 4. List and inspect tasks only on instances in the correct AZ
	taskPaginator := ecs.NewListTasksPaginator(ecsClient, &ecs.ListTasksInput{Cluster: &clusterName})
	var taskArns []string
	for taskPaginator.HasMorePages() {
		tasksOutput, err := taskPaginator.NextPage(ctx)
		if err != nil {
			log.Printf("error listing tasks for %s: %v", clusterName, err)
			return
		}
		taskArns = append(taskArns, tasksOutput.TaskArns...)
	}

	if len(taskArns) == 0 {
		return
	}

	describedTasks, err := ecsClient.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: &clusterName, Tasks: taskArns})
	if err != nil {
		log.Printf("error describing tasks for %s: %v", clusterName, err)
		return
	}

	taskDefsToInspect := make(map[string]bool)
	for _, task := range describedTasks.Tasks {
		if _, ok := ciArnsInAZ[*task.ContainerInstanceArn]; ok {
			taskDefsToInspect[*task.TaskDefinitionArn] = true
		}
	}

	var taskDefArnsToCheck []string

	for taskDefArn := range taskDefsToInspect {
		defOutput, err := ecsClient.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: &taskDefArn})
		if err != nil {
			log.Printf("error describing task def %s: %v", taskDefArn, err)
			continue
		}

		for _, vol := range defOutput.TaskDefinition.Volumes {
			if *vol.Name == volumeToCheck {
				log.Printf("Found volume '%s' in use by task def %s", volumeToCheck, taskDefArn)
				taskDefArnsToCheck = append(taskDefArnsToCheck, taskDefArn)
			}
		}
	}

	if len(taskDefArnsToCheck) == 0 {
		log.Printf("No tasks using volume '%s' found in cluster %s", volumeToCheck, clusterName)
		return
	}

	// https://docs.aws.amazon.com/AmazonECS/latest/developerguide/task-lifecycle-explanation.html
	stillRunningTaskState := map[string]struct{}{
		"RUNNING":   {},
		"PENDING":   {},
		"PROVISIONING":   {},
		"ACTIVATING":   {},
		"DEACTIVATING":   {},
		"STOPPING":   {},
		// "DEPROVISIONING":   {}, // I think this can be ignored
	}

	for _, taskDefArn := range taskDefArnsToCheck {
		// At the start of each iteration, check if a task that uses the volume was already found
		mu.Lock()
		if *volumeFound > 1 {
			mu.Unlock()
			return
		}
		mu.Unlock()
		for _, task := range describedTasks.Tasks {
			if *task.TaskDefinitionArn == taskDefArn {
				if _, ok := stillRunningTaskState[*task.LastStatus]; ok {
					// If the task is still in one of the state above, we can consider the volume in use
					mu.Lock()
					*volumeFound += 1
					log.Printf("Volume '%s' is in use by task %s in cluster %s", volumeToCheck, *task.TaskArn, clusterName)
					if *volumeFound > 1 {
						mu.Unlock()
						return
					}
					mu.Unlock()
				}
			}
		}
	}
}

func CheckForTasksWithVolumeInUse(volumeToCheck string, region string, availabilityZone string) (Status, error) {
	log.Println("Starting check for tasks using volume: ", volumeToCheck)

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
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
	volumeFound := 0

	// Check each cluster concurrently
	for _, clusterArn := range clustersOutput.ClusterArns {
		wg.Add(1)
		go checkCluster(ctx, cfg, clusterArn, availabilityZone, &volumeFound, &mu, &wg, volumeToCheck)
	}

	wg.Wait() // Wait for all checks to finish

	if volumeFound > 1 {
		return VolumeInUseError, fmt.Errorf("volume '%s' is currently in use by task", volumeToCheck)
	}

	return OK, nil
}

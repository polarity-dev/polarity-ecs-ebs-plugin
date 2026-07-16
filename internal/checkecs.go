package internal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/smithy-go"
)

type Status int

const (
	OK Status = iota
	ProcessingError
	VolumeInUseError
)

type TaskInfo struct {
	TaskArn       string
	ClusterArn    string
	Status        string
	DesiredStatus string
	InstanceID    string
}

type CheckResult struct {
	Status Status
	Err    error
	Tasks  []TaskInfo
}

// Check single cluster
func checkCluster(ctx context.Context, cfg aws.Config, clusterArn, targetAZ string, mu *sync.Mutex, wg *sync.WaitGroup, volumeToCheck string, tasks *[]TaskInfo) {
	defer wg.Done()

	ecsClient := ecs.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	clusterName := clusterArn[strings.LastIndex(clusterArn, "/")+1:]

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

	// 2. Describe container instances to get EC2 IDs
	describedCIs, err := ecsClient.DescribeContainerInstances(ctx, &ecs.DescribeContainerInstancesInput{Cluster: &clusterName, ContainerInstances: ciArns})
	if err != nil {
		log.Printf("error describing instances for %s: %v", clusterName, err)
		return
	}

	ec2IdToCiArn := make(map[string]string)
	ciArnToEc2Id := make(map[string]string)
	var ec2Ids []string
	for _, ci := range describedCIs.ContainerInstances {
		ec2IdToCiArn[*ci.Ec2InstanceId] = *ci.ContainerInstanceArn
		ciArnToEc2Id[*ci.ContainerInstanceArn] = *ci.Ec2InstanceId
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
		if task.ContainerInstanceArn != nil {
			if _, ok := ciArnsInAZ[*task.ContainerInstanceArn]; ok {
				taskDefsToInspect[*task.TaskDefinitionArn] = true
			}
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
		"RUNNING":      {},
		"PENDING":      {},
		"PROVISIONING": {},
		"ACTIVATING":   {},
		"DEACTIVATING": {},
		"STOPPING":     {},
	}

	for _, taskDefArn := range taskDefArnsToCheck {
		for _, task := range describedTasks.Tasks {
			if *task.TaskDefinitionArn == taskDefArn {
				if _, ok := stillRunningTaskState[*task.LastStatus]; ok {
					instanceID := ""
					if task.ContainerInstanceArn != nil {
						instanceID = ciArnToEc2Id[*task.ContainerInstanceArn]
					}
					desiredStatus := ""
					if task.DesiredStatus != nil {
						desiredStatus = *task.DesiredStatus
					}
					mu.Lock()
					*tasks = append(*tasks, TaskInfo{
						TaskArn:       *task.TaskArn,
						ClusterArn:    clusterArn,
						Status:        *task.LastStatus,
						DesiredStatus: desiredStatus,
						InstanceID:    instanceID,
					})
					log.Printf("Volume '%s' is in use by task %s (last: %s, desired: %s, instance: %s) in cluster %s", volumeToCheck, *task.TaskArn, *task.LastStatus, desiredStatus, instanceID, clusterName)
					mu.Unlock()
				}
			}
		}
	}
}

// CheckForTasksWithVolumeInUse checks all clusters for tasks using the given volume.
// Returns the list of tasks found so the caller can decide what to do.
func CheckForTasksWithVolumeInUse(volumeToCheck string, region string, availabilityZone string) CheckResult {
	log.Println("Starting check for tasks using volume: ", volumeToCheck)

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return CheckResult{Status: ProcessingError, Err: fmt.Errorf("error while creating AWS configuration: %v", err)}
	}

	ecsClient := ecs.NewFromConfig(cfg)

	log.Println("List all clusters in AZ...")
	clustersOutput, err := ecsClient.ListClusters(ctx, &ecs.ListClustersInput{})
	if err != nil {
		return CheckResult{Status: ProcessingError, Err: fmt.Errorf("cannot list clusters: %v", err)}
	}

	if len(clustersOutput.ClusterArns) == 0 {
		log.Println("No clusters found in this AZ.")
		return CheckResult{Status: OK}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var tasks []TaskInfo

	// Check each cluster concurrently
	for _, clusterArn := range clustersOutput.ClusterArns {
		wg.Add(1)
		go checkCluster(ctx, cfg, clusterArn, availabilityZone, &mu, &wg, volumeToCheck, &tasks)
	}

	wg.Wait()

	if len(tasks) == 0 {
		return CheckResult{Status: OK, Tasks: nil}
	}

	return CheckResult{Status: VolumeInUseError, Tasks: tasks, Err: fmt.Errorf("volume '%s' is in use by %d task(s)", volumeToCheck, len(tasks))}
}

// StopTask attempts to stop an ECS task. Returns nil on success.
func StopTask(region string, clusterArn string, taskArn string) error {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	ecsClient := ecs.NewFromConfig(cfg)

	clusterName := clusterArn[strings.LastIndex(clusterArn, "/")+1:]

	log.Printf("Stopping task %s in cluster %s", taskArn, clusterName)
	_, err = ecsClient.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: &clusterName,
		Task:    &taskArn,
		Reason:  aws.String("polarity-ecs-ebs-plugin: volume takeover for new task"),
	})
	if err != nil {
		return fmt.Errorf("failed to stop task %s: %w", taskArn, err)
	}

	log.Printf("StopTask issued for %s, waiting for STOPPED state...", taskArn)

	// Wait for the task to reach STOPPED state (poll every 2s, max 60s)
	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)

		describeOutput, err := ecsClient.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: &clusterName,
			Tasks:   []string{taskArn},
		})
		if err != nil {
			log.Printf("Warning: failed to describe task %s: %v", taskArn, err)
			continue
		}

		if len(describeOutput.Tasks) > 0 {
			status := describeOutput.Tasks[0].LastStatus
			if status != nil && (*status == "STOPPED" || *status == "DEPROVISIONING") {
				log.Printf("Task %s is now %s", taskArn, *status)
				return nil
			}
			log.Printf("Task %s is still in state %s, waiting...", taskArn, *status)
		}

		// Task not found = already gone
		if len(describeOutput.Failures) > 0 {
			for _, f := range describeOutput.Failures {
				if f.Reason != nil && *f.Reason == "MISSING" {
					log.Printf("Task %s no longer exists (MISSING), treating as stopped", taskArn)
					return nil
				}
			}
		}
	}

	return fmt.Errorf("task %s did not reach STOPPED state within 60s", taskArn)
}

// TerminateInstance terminates an EC2 instance. Last-resort fallback, gated off by
// default in the caller (see canTerminateInstances).
func TerminateInstance(region string, instanceID string) error {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	ec2Client := ec2.NewFromConfig(cfg)

	log.Printf("TERMINATING instance %s as last resort for volume recovery", instanceID)
	_, err = ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instance %s: %w", instanceID, err)
	}

	log.Printf("TerminateInstances issued for %s", instanceID)
	return nil
}

// lastStatus values where a task's container may still be live on the volume. A task
// being mounted is pre-RUNNING, so it is never matched.
var liveContainerStates = map[string]struct{}{
	"RUNNING":      {},
	"DEACTIVATING": {},
	"STOPPING":     {},
}

// StopCandidates returns the other tasks whose container may still be live on the
// volume and must be stopped before we can mount it here.
func StopCandidates(tasks []TaskInfo) []TaskInfo {
	var candidates []TaskInfo
	for _, t := range tasks {
		if _, live := liveContainerStates[t.Status]; live {
			candidates = append(candidates, t)
		}
	}
	return candidates
}

// IsAuthorizationError reports whether err is an AWS authorization failure (e.g. a
// missing IAM permission) rather than a runtime/unreachable-instance failure.
func IsAuthorizationError(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation", "MissingAuthenticationToken":
			return true
		}
	}
	return false
}

# polarity-ecs-ebs-plugin

## Description
This Docker plugin allows you to attach an existing **EBS volume** to an **ECS task** using the **EC2 launch type**.
It provides persistent storage beyond the lifecycle of a single task instantiation.

The plugin works by:
1. Attaching an EBS volume to the EC2 instance where ECS is starting the task.
2. Creating a corresponding Docker volume.
3. Attaching it to the ECS task when the container starts.

⚠️ The ECS container must be in the **same Availability Zone** as the EBS volume.
If the volume has no filesystem, one will be created with `mkfs.xfs`.


## Usage

In order to use the plugin you must:
1. Install it in the EC2 instances of your ECS cluster
2. Specify an EBS volume id and a mount path for the Docker volume in your container
3. Make sure your EC2 instance has the required IAM permissions to attach the EBS volume to the instance itself

### Installation

The plugin should be either already present in your AMI, or downloaded and installed from user data. We recommend using the [Amazon ECS-optimized Linux AMIs](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/retrieve-ecs-optimized_AMI.html) as a starting base.
```sh
# Install the Docker volume plugin
# For Intel (amd64)
curl -o polarity-ecs-ebs-plugin.tar.gz https://github.com/polarity-dev/polarity-ecs-ebs-plugin/releases/download/v0.1.0/polarity-ecs-ebs-plugin.amd64.tar.gz
# For ARM (arm64)
curl -o polarity-ecs-ebs-plugin.tar.gz https://github.com/polarity-dev/polarity-ecs-ebs-plugin/releases/download/v0.1.0/polarity-ecs-ebs-plugin.arm64.tar.gz

mkdir polarity-ecs-ebs-plugin
tar -xzf polarity-ecs-ebs-plugin.tar.gz -C polarity-ecs-ebs-plugin
docker plugin create polarity-ecs-ebs-plugin ./polarity-ecs-ebs-plugin
docker plugin enable polarity-ecs-ebs-plugin
```

### Task Definition

The plugin mounts the EBS volume with the given id to the desired container path.

- Task Definition example using CloudFormation yaml

```yaml
TaskDefinition:
    Type: AWS::ECS::TaskDefinition
    Properties:
      Volumes:
        - Name: <ebs-volume-id>
          DockerVolumeConfiguration:
            Scope: shared
            Autoprovision: true
            Driver: polarity-ecs-ebs-plugin
            Labels:
              Name: <ebs-volume-id>
      ContainerDefinitions:
        MountPoints:
          - SourceVolume: <ebs-volume-id>
            ContainerPath: <your desired path of your app in the container>

```
- Task Definition example using Terraform

```terraform
resource "aws_ecs_task_definition" "task_with_ebs" {
  volume {
    name = aws_ebs_volume.data.id
    docker_volume_configuration {
      scope         = "shared"
      autoprovision = true
      driver        = "polarity-ecs-ebs-plugin"
      labels = {
        Name = aws_ebs_volume.data.id
      }
    }
  }
  container_definitions = jsonencode([
    {
      mountPoints = [
        {
          sourceVolume  = aws_ebs_volume.data.id
          containerPath = var.container_mount_path
        }
      ]
    }
  ])
}
```

## Permissions
IAM Policy example. This should be applied to the IAM Role of the EC2 instances in the ECS cluster
```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Sid": "DockerPluginOps",
            "Action": [
                "ecs:ListClusters",
                "ecs:ListContainerInstances",
                "ecs:DescribeContainerInstances",
                "ecs:ListTasks",
                "ecs:DescribeTasks",
                "ecs:DescribeTaskDefinition",
                "ec2:DescribeInstances",
                "ec2:DescribeVolumes"
            ],
            "Effect": "Allow",
            "Resource": "*"
        },
				{
            "Sid": "DockerPluginWrite",
            "Action": [
                "ec2:DetachVolume",
                "ec2:AttachVolume"
            ],
            "Effect": "Allow",
            "Resource": "<volume_arn>"
        }
    ]
}
```


## Notes
When the task dies or is terminated by ECS, the volume is NOT automatically detached from the EC2: this is intentional to spin up a new instance of the container faster in case of failure or ECS service update.

## Development instructions
Docker plugins are not regular docker containers. They are just a folder with a `config.json` and a `rootfs`:
- `config.json` is the file that describes the plugin, where to find the binary of the plugin and what paths to mount
- `rootfs` is the isolated filesystem of the plugin, to comunicate with the host machine we need to mount the path that we want to work on (`/dev`)
- our binary will be located in `/rootfs/bin`


The plugin must be compiled separately for Intel and ARM architectures, and requires additional tools and files to function correctly.
When running on an EC2 instance, the plugin uses the AWS SDK, which relies on the IAM role of the host machine. However, this works only if the host has the necessary certificates to authenticate with AWS APIs.
Since the plugin operates in a completely isolated filesystem, it cannot access certificates or binaries (such as `lsblk` or `mkfs.xfs`) present on the host by default.
To resolve these issues, the plugin is built using Docker: all required certificates and tools are installed inside the container, and then the filesystem of the Docker image is exported. This ensures the plugin has everything it needs to work independently.

To develop on the plugin you can run
```sh
make dev
```
This will start a local sock with the server
You can also run `make health-check` to check if the server is responding

To test the full functionality of the plugin you should run `make debug-tar-amd64` and copy the `.tar.gz` file on your ECS cluster
This version will also create a log file in `/var/log/polarity-ecs-ebs.log`

To call manually the server on ECS cluster you should ssh into the cluster and then follow the installation guide.

Now your plugin will be enabled, the sock file will be located in `/var/run/docker/plugins/` in a folder with the plugin hash.
You just need to run something like this
```sh
curl -H "Content-Type: application/json" -XPOST -d '{ "Name": "test" }' --unix-socket ./pl-ebs.sock http://localhost/health
```

### Contribution
Non-exhaustive list of future improvements to be developed:
- [ ] Any number of ECS tasks can be attached to the same EBS volume, provided they reside in the same EC2 instance

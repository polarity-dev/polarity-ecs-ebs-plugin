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

### Requirements
- Go >= 1.20
- Docker >= 20.10
- GNU Make

### Building from source
You need to build the plugin separately for Intel and ARM. All required tools and certificates must be included in the isolated filesystem (`rootfs`).

**Build for Intel (amd64):**
```sh
make docker-build-amd64
```

**Build for ARM (arm64):**
```sh
make docker-build-arm64
```

To create the debug tarball:
```sh
make debug-tar-amd64
make debug-tar-arm64
```

### Run
To start the plugin locally for development:
```sh
make dev
```
This starts the local server and creates the socket file.
To check if the server is running:
```sh
make health-check
```

To test the full plugin, copy the `.tar.gz` file to your ECS cluster. The debug version creates a log at `/var/log/polarity-ecs-ebs.log`.

To manually call the server on ECS, SSH into the cluster and follow the install guide. When enabled, the socket file is in `/var/run/docker/plugins/`.
To check plugin health:
```sh
curl -H "Content-Type: application/json" -XPOST -d '{ "Name": "test" }' --unix-socket ./pl-ebs.sock http://localhost/health
```

### Publish
To publish a new version after making changes:
1. Push a new commit on `main` branch, this will trigger the github action that uploads the plugin to AWS S3.
2. Create a new tag in the format `vX.Y.Z` (e.g. `v0.1.1`) and push it to github. This will trigger the github action that creates the release in github and uploads the tarballs to the release page.

### Variables / Configuration
- `config.json`: describes plugin behavior, binary path, and mounts, it will be automatically generated when building the plugin.
- Compile-time variables (if used):
  - `Debug`: enable debug logs
- Important paths:
  - Plugin binary must be in `/rootfs/bin`
  - Certificates and required tools (like `lsblk`, `mkfs.xfs`) must be in `rootfs`
- Log file: `/var/log/polarity-ecs-ebs.log` (debug version only)

---

### Contribution
Non-exhaustive list of future improvements to be developed:
- [ ] Any number of ECS tasks can be attached to the same EBS volume, provided they reside in the same EC2 instance

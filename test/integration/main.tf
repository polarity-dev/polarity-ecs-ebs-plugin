terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.0"
    }
  }
}

provider "aws" {
  region = var.aws_region
  default_tags {
    tags = {
      Project   = "ecs-ebs-plugin-integration-test"
      ManagedBy = "terraform"
    }
  }
}


### Variables ###

variable "aws_region" {
  type    = string
  default = "eu-west-1"
}

variable "name_prefix" {
  type    = string
  default = "ebs-plugin-it"
}

variable "vpc_cidr" {
  type    = string
  default = "10.99.0.0/16"
}

variable "subnet_cidr" {
  type    = string
  default = "10.99.1.0/24"
}

# arm64 (Graviton). Must match the plugin build arch in the workflow.
variable "instance_type" {
  type    = string
  default = "t4g.small"
}

variable "desired_instances" {
  type    = number
  default = 2
}

# Local path to the built plugin tarball; uploaded to an ephemeral bucket by TF.
variable "plugin_tarball_path" {
  type = string
}

variable "mongo_image" {
  type    = string
  default = "mongo:7"
}

variable "deployment_maximum_percent" {
  type    = number
  default = 200
}

# Toggle off to test the fail-safe fallback (no force-detach without ecs:StopTask).
variable "grant_stop_task" {
  type    = bool
  default = true
}

### Data Sources ###

data "aws_caller_identity" "current" {}

data "aws_availability_zones" "available" {
  state = "available"
}

data "aws_ssm_parameter" "ecs_ami" {
  name = "/aws/service/ecs/optimized-ami/amazon-linux-2023/arm64/recommended/image_id"
}

locals {
  az            = data.aws_availability_zones.available.names[0]
  instance_attr = "ebsPluginIt"
}


### Plugin Artifact Bucket (ephemeral) ###

resource "random_id" "suffix" {
  byte_length = 4
}

resource "aws_s3_bucket" "plugin" {
  bucket        = "${var.name_prefix}-${random_id.suffix.hex}"
  force_destroy = true
  tags          = { Name = var.name_prefix }
}

resource "aws_s3_object" "plugin" {
  bucket = aws_s3_bucket.plugin.id
  key    = "plugin.tar.gz"
  source = var.plugin_tarball_path
  etag   = filemd5(var.plugin_tarball_path)
}


### VPC (single public subnet, no NAT) ###

resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_hostnames = true
  enable_dns_support   = true
  tags                 = { Name = var.name_prefix }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = var.name_prefix }
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.this.id
  cidr_block              = var.subnet_cidr
  availability_zone       = local.az
  map_public_ip_on_launch = true
  tags                    = { Name = "${var.name_prefix}-public" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }
  tags = { Name = "${var.name_prefix}-public" }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}


### Security Group ###

resource "aws_security_group" "instance" {
  name        = "${var.name_prefix}-instance"
  description = "ECS instances"
  vpc_id      = aws_vpc.this.id

  ingress {
    from_port   = 27017
    to_port     = 27017
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
  tags = { Name = "${var.name_prefix}-instance" }
}


### Instance IAM Role ###

resource "aws_iam_role" "instance" {
  name = "${var.name_prefix}-instance"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "ecs" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonEC2ContainerServiceforEC2Role"
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy" "plugin_base" {
  name = "${var.name_prefix}-plugin-base"
  role = aws_iam_role.instance.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "VolumeOps"
        Effect = "Allow"
        Action = [
          "ec2:DescribeVolumes",
          "ec2:DescribeInstances",
          "ec2:DescribeTags",
          "ec2:CreateTags",
          "ec2:AttachVolume",
          "ec2:DetachVolume"
        ]
        Resource = "*"
      },
      {
        Sid    = "EcsRead"
        Effect = "Allow"
        Action = [
          "ecs:ListClusters",
          "ecs:ListContainerInstances",
          "ecs:DescribeContainerInstances",
          "ecs:ListTasks",
          "ecs:DescribeTasks",
          "ecs:DescribeTaskDefinition"
        ]
        Resource = "*"
      },
      {
        Sid      = "PluginDownload"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:ListBucket"]
        Resource = [aws_s3_bucket.plugin.arn, "${aws_s3_bucket.plugin.arn}/*"]
      }
    ]
  })
}

resource "aws_iam_role_policy" "plugin_stop_task" {
  count = var.grant_stop_task ? 1 : 0
  name  = "${var.name_prefix}-plugin-stop-task"
  role  = aws_iam_role.instance.name
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Sid = "StopTask", Effect = "Allow", Action = ["ecs:StopTask"], Resource = "*" }]
  })
}

resource "aws_iam_instance_profile" "instance" {
  name = "${var.name_prefix}-instance"
  role = aws_iam_role.instance.name
}


### Launch Template + ASG ###

resource "aws_launch_template" "this" {
  name          = var.name_prefix
  image_id      = data.aws_ssm_parameter.ecs_ami.value
  instance_type = var.instance_type

  iam_instance_profile {
    name = aws_iam_instance_profile.instance.name
  }

  vpc_security_group_ids = [aws_security_group.instance.id]

  user_data = base64encode(<<-EOF
    #!/bin/bash
    set -euxo pipefail
    dnf install -y nvme-cli jq

    cat >> /etc/ecs/ecs.config <<CFG
    ECS_CLUSTER=${aws_ecs_cluster.this.name}
    ECS_CONTAINER_STOP_TIMEOUT=10s
    ECS_ENGINE_TASK_CLEANUP_WAIT_DURATION=1m
    ECS_INSTANCE_ATTRIBUTES={"${local.instance_attr}":"true"}
    CFG

    # Install the plugin build under test
    mkdir -p /tmp/plugin
    aws s3 cp s3://${aws_s3_bucket.plugin.id}/${aws_s3_object.plugin.key} /tmp/plugin/plugin.tar.gz --region ${var.aws_region}
    tar -xzf /tmp/plugin/plugin.tar.gz -C /tmp/plugin
    docker plugin create polarity-ecs-ebs-plugin /tmp/plugin
    docker plugin enable polarity-ecs-ebs-plugin
    rm -rf /tmp/plugin
    EOF
  )

  tag_specifications {
    resource_type = "instance"
    tags          = { Name = var.name_prefix }
  }
}

resource "aws_autoscaling_group" "this" {
  name                  = var.name_prefix
  min_size              = 1
  max_size              = var.desired_instances + 1
  desired_capacity      = var.desired_instances
  vpc_zone_identifier   = [aws_subnet.public.id]
  health_check_type     = "EC2"
  protect_from_scale_in = false

  launch_template {
    id      = aws_launch_template.this.id
    version = "$Latest"
  }

  tag {
    key                 = "Name"
    value               = var.name_prefix
    propagate_at_launch = true
  }
}


### ECS Cluster ###

resource "aws_ecs_cluster" "this" {
  name = var.name_prefix
}

resource "aws_cloudwatch_log_group" "this" {
  name              = "/${var.name_prefix}"
  retention_in_days = 1
}


### Task Roles ###

resource "aws_iam_role" "task_execution" {
  name = "${var.name_prefix}-task-execution"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ecs-tasks.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

resource "aws_iam_role_policy_attachment" "task_execution" {
  role       = aws_iam_role.task_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role" "task" {
  name = "${var.name_prefix}-task"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ecs-tasks.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

# ECS Exec channel — the test runner reads/writes the MongoDB sentinel via execute-command.
resource "aws_iam_role_policy" "task_exec_channel" {
  name = "${var.name_prefix}-task-exec"
  role = aws_iam_role.task.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "ssmmessages:CreateControlChannel",
        "ssmmessages:CreateDataChannel",
        "ssmmessages:OpenControlChannel",
        "ssmmessages:OpenDataChannel"
      ]
      Resource = "*"
    }]
  })
}


### EBS Volume ###

resource "aws_ebs_volume" "data" {
  availability_zone = local.az
  size              = 5
  type              = "gp3"
  tags              = { Name = "${var.name_prefix}-data" }
}


### Task Definition ###

resource "aws_ecs_task_definition" "mongo" {
  family                   = "${var.name_prefix}-mongo"
  network_mode             = "awsvpc"
  requires_compatibilities = ["EC2"]
  execution_role_arn       = aws_iam_role.task_execution.arn
  task_role_arn            = aws_iam_role.task.arn

  volume {
    name = aws_ebs_volume.data.id
    docker_volume_configuration {
      scope         = "shared"
      autoprovision = true
      driver        = "polarity-ecs-ebs-plugin"
      labels        = { Name = aws_ebs_volume.data.id }
    }
  }

  container_definitions = jsonencode([{
    name              = "mongo"
    image             = var.mongo_image
    essential         = true
    memory            = 512
    memoryReservation = 256
    linuxParameters   = { initProcessEnabled = true }
    mountPoints = [{
      sourceVolume  = aws_ebs_volume.data.id
      containerPath = "/data/db"
    }]
    portMappings = [{ containerPort = 27017, protocol = "tcp" }]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.this.name
        "awslogs-region"        = var.aws_region
        "awslogs-stream-prefix" = "mongo"
      }
    }
  }])
}


### ECS Service ###

resource "aws_ecs_service" "mongo" {
  name                               = "${var.name_prefix}-mongo"
  cluster                            = aws_ecs_cluster.this.id
  task_definition                    = aws_ecs_task_definition.mongo.arn
  desired_count                      = 1
  launch_type                        = "EC2"
  enable_execute_command             = true
  deployment_minimum_healthy_percent = 0
  deployment_maximum_percent         = var.deployment_maximum_percent

  network_configuration {
    subnets         = [aws_subnet.public.id]
    security_groups = [aws_security_group.instance.id]
  }

  placement_constraints {
    type       = "memberOf"
    expression = "attribute:${local.instance_attr} == true"
  }

  ordered_placement_strategy {
    type  = "spread"
    field = "instanceId"
  }
}


### Outputs ###

output "region" { value = var.aws_region }
output "cluster_name" { value = aws_ecs_cluster.this.name }
output "service_name" { value = aws_ecs_service.mongo.name }
output "volume_id" { value = aws_ebs_volume.data.id }
output "grant_stop_task" { value = var.grant_stop_task }
output "deployment_maximum_percent" { value = var.deployment_maximum_percent }

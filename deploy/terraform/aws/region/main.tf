# One region of the fleet: a VPC with a private node subnet, NAT for the
# binaries download and SSM, and an autoscaling group of one-node instances.
# The home region also hosts the coordinator. Created in every supported
# region; a region with no nodes and no coordinator costs nothing (no NAT).
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

variable "nodes" { type = number }
variable "coordinator" { type = bool }
variable "cidr" { type = string }
variable "instance_type" { type = string }
variable "coordinator_type" { type = string }
variable "spot" { type = bool }
variable "instance_profile" { type = string }
variable "cloud_init" { type = string }

locals {
  active = var.nodes > 0 || var.coordinator
}

data "aws_availability_zones" "az" {
  state = "available"
}
data "aws_ami" "al2023_arm" {
  most_recent = true
  owners      = ["amazon"]
  filter {
    name   = "name"
    values = ["al2023-ami-*-arm64"]
  }
}
data "aws_region" "r" {}

resource "aws_vpc" "tb" {
  cidr_block           = var.cidr
  enable_dns_hostnames = true
  tags                 = { Name = "topdisc-testbed" }
}
resource "aws_subnet" "nodes" {
  vpc_id            = aws_vpc.tb.id
  cidr_block        = cidrsubnet(var.cidr, 2, 0) # /18: 16k addresses
  availability_zone = data.aws_availability_zones.az.names[0]
}
resource "aws_subnet" "public" {
  vpc_id            = aws_vpc.tb.id
  cidr_block        = cidrsubnet(var.cidr, 8, 255)
  availability_zone = data.aws_availability_zones.az.names[0]
}
resource "aws_security_group" "intra" {
  vpc_id = aws_vpc.tb.id
  ingress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["10.0.0.0/8"] # this VPC and the peered regions
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_internet_gateway" "igw" {
  count  = local.active ? 1 : 0
  vpc_id = aws_vpc.tb.id
}
resource "aws_eip" "nat" {
  count  = local.active ? 1 : 0
  domain = "vpc"
}
resource "aws_nat_gateway" "nat" {
  count         = local.active ? 1 : 0
  allocation_id = aws_eip.nat[0].id
  subnet_id     = aws_subnet.public.id
  depends_on    = [aws_internet_gateway.igw]
}
resource "aws_route_table" "public" {
  count  = local.active ? 1 : 0
  vpc_id = aws_vpc.tb.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.igw[0].id
  }
}
resource "aws_route_table_association" "public" {
  count          = local.active ? 1 : 0
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public[0].id
}
resource "aws_route" "private_default" {
  count                  = local.active ? 1 : 0
  route_table_id         = aws_vpc.tb.main_route_table_id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.nat[0].id
}
resource "aws_vpc_endpoint" "s3" {
  vpc_id          = aws_vpc.tb.id
  service_name    = "com.amazonaws.${data.aws_region.r.name}.s3"
  route_table_ids = [aws_vpc.tb.main_route_table_id]
}

resource "aws_instance" "coordinator" {
  count                  = var.coordinator ? 1 : 0
  ami                    = data.aws_ami.al2023_arm.id
  instance_type          = var.coordinator_type
  subnet_id              = aws_subnet.nodes.id
  vpc_security_group_ids = [aws_security_group.intra.id]
  iam_instance_profile   = var.instance_profile
  user_data              = var.cloud_init
  root_block_device {
    volume_size = 20
    volume_type = "gp3"
  }
  tags = { Name = "topdisc-coordinator", role = "coordinator" }
}

resource "aws_launch_template" "node" {
  count         = var.nodes > 0 ? 1 : 0
  name_prefix   = "topdisc-node-"
  image_id      = data.aws_ami.al2023_arm.id
  instance_type = var.instance_type
  user_data     = base64encode(var.cloud_init)
  iam_instance_profile { name = var.instance_profile }
  network_interfaces {
    subnet_id       = aws_subnet.nodes.id
    security_groups = [aws_security_group.intra.id]
  }
  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      volume_size = 4
      volume_type = "gp3"
    }
  }
  dynamic "instance_market_options" {
    for_each = var.spot ? [1] : []
    content {
      market_type = "spot"
      spot_options { spot_instance_type = "one-time" }
    }
  }
  tag_specifications {
    resource_type = "instance"
    tags          = { Name = "topdisc-node", role = "host" }
  }
}
resource "aws_autoscaling_group" "nodes" {
  count               = var.nodes > 0 ? 1 : 0
  name                = "topdisc-nodes"
  desired_capacity    = var.nodes
  min_size            = 0
  max_size            = var.nodes
  vpc_zone_identifier = [aws_subnet.nodes.id]
  launch_template {
    id      = aws_launch_template.node[0].id
    version = "$Latest"
  }
  wait_for_capacity_timeout = "0"
}

output "vpc_id" { value = aws_vpc.tb.id }
output "route_table_id" { value = aws_vpc.tb.main_route_table_id }
output "cidr" { value = var.cidr }
output "active" { value = local.active }
output "coordinator_id" { value = one(aws_instance.coordinator[*].id) }
output "coordinator_ip" { value = one(aws_instance.coordinator[*].private_ip) }

# One node per instance: an autoscaling group of N small Graviton instances
# plus one coordinator, all on a private subnet (no public IPv4, which is
# billed per address). Shell access through SSM.
terraform {
  required_providers { aws = { source = "hashicorp/aws", version = "~> 5.0" } }
}
provider "aws" { region = var.region }

data "aws_ami" "al2023_arm" {
  most_recent = true
  owners      = ["amazon"]
  filter {
    name   = "name"
    values = ["al2023-ami-*-arm64"]
  }
}

resource "aws_vpc" "tb" {
  cidr_block           = "10.10.0.0/16"
  enable_dns_hostnames = true
  tags                 = { Name = "topdisc-testbed" }
}
resource "aws_subnet" "nodes" {
  vpc_id            = aws_vpc.tb.id
  cidr_block        = "10.10.0.0/18" # 16k addresses
  availability_zone = var.az
}
resource "aws_security_group" "intra" {
  vpc_id = aws_vpc.tb.id
  ingress {
    from_port = 0
    to_port   = 0
    protocol  = "-1"
    self      = true
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# Binaries download without public addresses: NAT gateway.
resource "aws_internet_gateway" "igw" { vpc_id = aws_vpc.tb.id }
resource "aws_subnet" "public" {
  vpc_id            = aws_vpc.tb.id
  cidr_block        = "10.10.255.0/24"
  availability_zone = var.az
}
resource "aws_eip" "nat" { domain = "vpc" }
resource "aws_nat_gateway" "nat" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public.id
}
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.tb.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.igw.id
  }
}
resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}
resource "aws_route" "private_default" {
  route_table_id         = aws_vpc.tb.main_route_table_id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.nat.id
}

resource "aws_iam_role" "ssm" {
  name               = "topdisc-testbed-ssm"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }] })
}
resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.ssm.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}
# The coordinator lists the fleet to build its inventory.
resource "aws_iam_role_policy_attachment" "ec2_read" {
  role       = aws_iam_role.ssm.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ReadOnlyAccess"
}
resource "aws_iam_instance_profile" "ssm" {
  name = "topdisc-testbed-ssm"
  role = aws_iam_role.ssm.name
}

locals {
  cloud_init = templatefile("${path.module}/../../cloud-init.yaml.tftpl", { binaries_url = var.binaries_url })
}

resource "aws_instance" "coordinator" {
  ami                    = data.aws_ami.al2023_arm.id
  instance_type          = var.coordinator_type
  subnet_id              = aws_subnet.nodes.id
  vpc_security_group_ids = [aws_security_group.intra.id]
  iam_instance_profile   = aws_iam_instance_profile.ssm.name
  user_data              = local.cloud_init
  root_block_device {
    volume_size = 20
    volume_type = "gp3"
  }
  tags = { Name = "topdisc-coordinator", role = "coordinator" }
}

resource "aws_launch_template" "node" {
  name_prefix   = "topdisc-node-"
  image_id      = data.aws_ami.al2023_arm.id
  instance_type = var.instance_type
  user_data     = base64encode(local.cloud_init)
  iam_instance_profile { name = aws_iam_instance_profile.ssm.name }
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
  name                = "topdisc-nodes"
  desired_capacity    = var.nodes
  min_size            = 0
  max_size            = var.nodes
  vpc_zone_identifier = [aws_subnet.nodes.id]
  launch_template {
    id      = aws_launch_template.node.id
    version = "$Latest"
  }
  wait_for_capacity_timeout = "0"
}

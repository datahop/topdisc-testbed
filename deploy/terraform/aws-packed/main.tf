# Packed hosts on AWS, the Google Cloud layout: an instance per
# `vnodes_per_machine` nodes, each node in its own netns on the host's bridge
# (pkg/host), plus a coordinator, all in one subnet of one availability zone.
# Host i owns 10.(100+i).0.0/16 and a route table entry sends that /16 to its
# network interface; the hosts forward and run without the source/dest
# check, so the VPC delivers packets addressed to their nodes. Every instance
# has a public IPv4 for the AWS APIs (S3, SSM) only; the security group lets
# nothing in from outside the VPC. Shell access is through SSM.
terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}
provider "aws" {
  region = var.region
}

data "aws_ami" "al2023" {
  most_recent = true
  owners      = ["amazon"]
  filter {
    name   = "name"
    values = ["al2023-ami-2023*-kernel-*-x86_64"] # not the minimal image: it has no SSM agent
  }
}

resource "aws_vpc" "tb" {
  cidr_block = var.subnet
  tags       = { Name = "topdisc-packed" }
}
resource "aws_internet_gateway" "tb" {
  vpc_id = aws_vpc.tb.id
  tags   = { Name = "topdisc-packed" }
}
resource "aws_subnet" "hosts" {
  vpc_id                  = aws_vpc.tb.id
  cidr_block              = var.subnet
  availability_zone       = var.zone
  map_public_ip_on_launch = true
  tags                    = { Name = "topdisc-packed" }
}
resource "aws_route_table" "tb" {
  vpc_id = aws_vpc.tb.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.tb.id
  }
  tags = { Name = "topdisc-packed" }
}
resource "aws_route_table_association" "hosts" {
  subnet_id      = aws_subnet.hosts.id
  route_table_id = aws_route_table.tb.id
}
resource "aws_route" "host" {
  count                  = var.hosts
  route_table_id         = aws_route_table.tb.id
  destination_cidr_block = "10.${100 + count.index}.0.0/16"
  network_interface_id   = aws_instance.host[count.index].primary_network_interface_id
}
resource "aws_security_group" "tb" {
  name   = "topdisc-packed"
  vpc_id = aws_vpc.tb.id
  ingress {
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = [var.subnet, "10.0.0.0/8"]
  }
  egress {
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# Binaries in, run archives out. Destroyed with the deployment:
# aws-packed.sh down copies the archives first.
resource "aws_s3_bucket" "tb" {
  bucket_prefix = "topdisc-packed-"
  force_destroy = true
}

locals {
  assume = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }] })
}
resource "aws_iam_role" "host" {
  name               = "topdisc-packed-host"
  assume_role_policy = local.assume
}
resource "aws_iam_role" "coordinator" {
  name               = "topdisc-packed-coordinator"
  assume_role_policy = local.assume
}
resource "aws_iam_role_policy_attachment" "ssm" {
  for_each   = { host = aws_iam_role.host.name, coordinator = aws_iam_role.coordinator.name }
  role       = each.value
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}
resource "aws_iam_role_policy" "host" {
  name = "topdisc-binaries"
  role = aws_iam_role.host.id
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Effect = "Allow", Action = ["s3:GetObject"], Resource = "${aws_s3_bucket.tb.arn}/*" },
    { Effect = "Allow", Action = ["s3:ListBucket"], Resource = aws_s3_bucket.tb.arn },
  ] })
}
# The coordinator lists the hosts for the inventory and terminates them, and
# only them, at the spending cap.
resource "aws_iam_role_policy" "coordinator" {
  name = "topdisc-coordinator"
  role = aws_iam_role.coordinator.id
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Effect = "Allow", Action = ["s3:GetObject", "s3:PutObject"], Resource = "${aws_s3_bucket.tb.arn}/*" },
    { Effect = "Allow", Action = ["s3:ListBucket"], Resource = aws_s3_bucket.tb.arn },
    { Effect = "Allow", Action = ["ec2:DescribeInstances"], Resource = "*" },
    { Effect = "Allow", Action = ["ec2:TerminateInstances"], Resource = "*", Condition = { StringEquals = { "ec2:ResourceTag/topdisc" = "host" } } },
  ] })
}
resource "aws_iam_instance_profile" "host" {
  name = "topdisc-packed-host"
  role = aws_iam_role.host.name
}
resource "aws_iam_instance_profile" "coordinator" {
  name = "topdisc-packed-coordinator"
  role = aws_iam_role.coordinator.name
}

locals {
  init = { for role in ["host", "coordinator"] : role => templatefile("${path.module}/../../aws-packed-init.sh.tftpl", {
    role = role, bucket = aws_s3_bucket.tb.id, region = var.region, max_hours = var.max_hours
  }) }
}

resource "aws_instance" "host" {
  count                       = var.hosts
  ami                         = data.aws_ami.al2023.id
  instance_type               = var.instance_type
  subnet_id                   = aws_subnet.hosts.id
  vpc_security_group_ids      = [aws_security_group.tb.id]
  iam_instance_profile        = aws_iam_instance_profile.host.name
  source_dest_check           = false
  user_data                   = local.init["host"]
  user_data_replace_on_change = true
  root_block_device {
    volume_size = var.host_disk_gb
    volume_type = "gp3"
  }
  metadata_options {
    http_tokens = "required"
  }
  tags       = { Name = "topdisc-host-${count.index}", topdisc = "host" }
  depends_on = [aws_iam_role_policy.host, aws_iam_role_policy_attachment.ssm]
}

resource "aws_instance" "coordinator" {
  ami                         = data.aws_ami.al2023.id
  instance_type               = var.coordinator_type
  subnet_id                   = aws_subnet.hosts.id
  vpc_security_group_ids      = [aws_security_group.tb.id]
  iam_instance_profile        = aws_iam_instance_profile.coordinator.name
  user_data                   = local.init["coordinator"]
  user_data_replace_on_change = true
  root_block_device {
    volume_size = var.coordinator_disk_gb
    volume_type = "gp3"
  }
  metadata_options {
    http_tokens = "required"
  }
  tags       = { Name = "topdisc-coordinator", topdisc = "coordinator" }
  depends_on = [aws_iam_role_policy.coordinator, aws_iam_role_policy_attachment.ssm]
}

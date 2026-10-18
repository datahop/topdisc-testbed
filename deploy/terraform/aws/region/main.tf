# One region of the fleet: a VPC and an autoscaling group of one-node
# instances. With public_ips every instance has a public IPv4, the subnets
# route through the internet gateway, and the nodes talk on the public
# addresses. Without it the nodes are private: they reach S3 (binaries,
# results) through the gateway endpoint, need nothing else outside, and talk
# over the peered VPCs; only the coordinator has a public address, for SSM
# and the AWS APIs. The coordinator always drives the hostagents over the
# private addresses. The home region also hosts the coordinator. Created in
# every supported region; a region with no nodes and no coordinator costs
# nothing.
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
variable "instance_types" { type = list(string) }
variable "coordinator_type" { type = string }
variable "spot" { type = bool }
variable "instance_profile" { type = string }
variable "cloud_init_vars" { type = map(string) } # cloud-init template inputs; the module adds region and binaries_s3
variable "public_ips" { type = bool }

locals {
  active = var.nodes > 0 || var.coordinator
  # Private nodes reach S3 through the gateway endpoint, which serves this
  # region's buckets only, so every region away from home gets a mirror of
  # the binaries; the home region's nodes read the bucket the coordinator uses.
  binaries_s3 = var.coordinator ? var.cloud_init_vars.binaries_s3 : (local.active ? "s3://${aws_s3_bucket.mirror[0].id}/topdisc-linux-arm64.tgz" : "")
  cloud_init  = templatefile("${path.module}/../../../cloud-init.yaml.tftpl", merge(var.cloud_init_vars, { region = data.aws_region.r.name, binaries_s3 = local.binaries_s3 }))
}

data "aws_availability_zones" "az" {
  state = "available"
}
data "aws_ami" "al2023_arm" {
  most_recent = true
  owners      = ["amazon"]
  filter {
    name   = "name"
    values = ["al2023-ami-2023*-kernel-*-arm64"] # not the minimal image: it has no SSM agent
  }
}
data "aws_region" "r" {}

resource "aws_vpc" "tb" {
  cidr_block           = var.cidr
  enable_dns_hostnames = true
  tags                 = { Name = "topdisc-testbed" }
}
# One node subnet per AZ (/20 each), so spot can draw on the whole region.
resource "aws_subnet" "nodes" {
  count                   = min(length(data.aws_availability_zones.az.names), 8)
  vpc_id                  = aws_vpc.tb.id
  cidr_block              = cidrsubnet(var.cidr, 4, count.index)
  availability_zone       = data.aws_availability_zones.az.names[count.index]
  map_public_ip_on_launch = var.public_ips
}
resource "aws_security_group" "intra" {
  vpc_id = aws_vpc.tb.id
  ingress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["10.0.0.0/8"] # this VPC and the peered regions
  }
  # With public addresses, node traffic comes from the other nodes' public
  # addresses, which are not known in advance.
  dynamic "ingress" {
    for_each = var.public_ips ? ["udp", "tcp"] : []
    content {
      from_port   = 30300
      to_port     = 30999
      protocol    = ingress.value
      cidr_blocks = ["0.0.0.0/0"]
    }
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
# The default route goes to the internet gateway in both layouts: only
# instances with a public address can use it, so private nodes stay offline.
resource "aws_route" "default" {
  count                  = local.active ? 1 : 0
  route_table_id         = aws_vpc.tb.main_route_table_id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.igw[0].id
}
moved {
  from = aws_route.private_default
  to   = aws_route.default
}
resource "aws_s3_bucket" "mirror" {
  count         = local.active && !var.coordinator ? 1 : 0
  bucket_prefix = "topdisc-testbed-"
  force_destroy = true
}
resource "aws_vpc_endpoint" "s3" {
  vpc_id          = aws_vpc.tb.id
  service_name    = "com.amazonaws.${data.aws_region.r.name}.s3"
  route_table_ids = [aws_vpc.tb.main_route_table_id]
}

resource "aws_instance" "coordinator" {
  # scale-down.sh ends with `shutdown -h now`: with this the halt terminates the
  # instance without an API call
  instance_initiated_shutdown_behavior = "terminate"
  count                                = var.coordinator ? 1 : 0
  ami                                  = data.aws_ami.al2023_arm.id
  instance_type                        = var.coordinator_type
  subnet_id                            = aws_subnet.nodes[0].id
  associate_public_ip_address          = true # SSM and the AWS APIs, with no NAT
  vpc_security_group_ids               = [aws_security_group.intra.id]
  iam_instance_profile                 = var.instance_profile
  user_data                            = local.cloud_init
  # A restarted instance does not re-run cloud-init; replace it instead.
  user_data_replace_on_change = true
  root_block_device {
    volume_size = 20
    volume_type = "gp3"
  }
  tags = { Name = "topdisc-coordinator", role = "coordinator" }
}

resource "aws_launch_template" "node" {
  count       = var.nodes > 0 ? 1 : 0
  name_prefix = "topdisc-node-"
  image_id    = data.aws_ami.al2023_arm.id
  user_data   = base64encode(local.cloud_init)
  iam_instance_profile { name = var.instance_profile }
  network_interfaces {
    associate_public_ip_address = var.public_ips
    security_groups             = [aws_security_group.intra.id]
  }
  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      volume_size = 8 # the AL2023 root snapshot is 8 GB
      volume_type = "gp3"
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
  vpc_zone_identifier = aws_subnet.nodes[*].id
  mixed_instances_policy {
    launch_template {
      launch_template_specification {
        launch_template_id = aws_launch_template.node[0].id
        version            = "$Latest"
      }
      dynamic "override" {
        for_each = var.instance_types
        content { instance_type = override.value }
      }
    }
    instances_distribution {
      on_demand_base_capacity                  = 0
      on_demand_percentage_above_base_capacity = var.spot ? 0 : 100
      spot_allocation_strategy                 = "price-capacity-optimized"
    }
  }
  wait_for_capacity_timeout = "0"
}

output "vpc_id" { value = aws_vpc.tb.id }
output "route_table_id" { value = aws_vpc.tb.main_route_table_id }
output "cidr" { value = var.cidr }
output "active" { value = local.active }
output "coordinator_id" { value = one(aws_instance.coordinator[*].id) }
output "coordinator_ip" { value = one(aws_instance.coordinator[*].private_ip) }
output "mirror_bucket" { value = one(aws_s3_bucket.mirror[*].id) }

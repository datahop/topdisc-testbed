# Offline: the mock provider creates nothing and needs no credentials.
mock_provider "aws" {}

variables {
  hosts = 1
}

run "default" {
  command = apply
  assert {
    condition = (aws_iam_role.host.name == "topdisc-packed-host" && aws_iam_role.coordinator.name == "topdisc-packed-coordinator"
    && aws_iam_instance_profile.host.name == "topdisc-packed-host" && aws_iam_instance_profile.coordinator.name == "topdisc-packed-coordinator")
    error_message = "IAM names changed"
  }
  assert {
    condition     = aws_security_group.tb.name == "topdisc-packed" && aws_s3_bucket.tb.bucket_prefix == "topdisc-packed-" && aws_vpc.tb.tags == tomap({ Name = "topdisc-packed" })
    error_message = "SG, bucket or VPC names changed"
  }
  assert {
    condition     = aws_instance.host[0].tags == tomap({ Name = "topdisc-host-0", topdisc = "host" }) && aws_instance.coordinator.tags == tomap({ Name = "topdisc-coordinator", topdisc = "coordinator" })
    error_message = "instance tags changed"
  }
  assert {
    condition     = try(aws_security_group.tb.tags["topdisc-deploy"], null) == null && try(aws_s3_bucket.tb.tags["topdisc-deploy"], null) == null
    error_message = "un-ID'd deployment tagged topdisc-deploy"
  }
  assert {
    condition     = !strcontains(aws_iam_role_policy.coordinator.policy, "topdisc-deploy") && !strcontains(aws_instance.coordinator.user_data, "topdisc-deploy")
    error_message = "un-ID'd policy or user_data mentions topdisc-deploy"
  }
}

# EC2 tracks no TCP or UDP flow that a 0.0.0.0/0 rule allows both ways; the
# network ACL is then all that keeps the internet out.
run "untracked" {
  command = apply
  assert {
    condition = (anytrue([for r in aws_security_group.tb.ingress : r.protocol == "-1" && contains(r.cidr_blocks, "0.0.0.0/0")])
    && anytrue([for r in aws_security_group.tb.egress : r.protocol == "-1" && contains(r.cidr_blocks, "0.0.0.0/0")]))
    error_message = "node flows would be tracked: the security group must allow everything from and to 0.0.0.0/0"
  }
  assert {
    condition     = aws_network_acl.tb.vpc_id == aws_vpc.tb.id && aws_network_acl.tb.subnet_ids == toset([aws_subnet.hosts.id])
    error_message = "the network ACL does not guard the hosts' subnet"
  }
  assert {
    condition = alltrue([for r in aws_network_acl.tb.ingress : r.action == "deny" || r.cidr_block == aws_subnet.hosts.cidr_block
      || (r.cidr_block == "10.0.0.0/8" && r.protocol == "-1") || (contains(["tcp", "6"], r.protocol) && r.from_port == 32768 && r.to_port == 60999)
    || (contains(["icmp", "1"], r.protocol) && r.icmp_type == 3 && r.icmp_code == 4)])
    error_message = "the network ACL lets the internet reach more than the ephemeral TCP ports and ICMP fragmentation-needed"
  }
  assert {
    condition = alltrue([for port in [22, 9000] : anytrue([for d in aws_network_acl.tb.ingress : d.action == "deny" && contains(["tcp", "6"], d.protocol) && d.cidr_block == "0.0.0.0/0"
      && d.from_port <= port && d.to_port >= port && alltrue([for r in aws_network_acl.tb.ingress : r.rule_no > d.rule_no
    if r.action == "allow" && r.cidr_block != aws_subnet.hosts.cidr_block])])])
    error_message = "sshd (22) or the hostagent (9000) is not denied ahead of every allow but the subnet's"
  }
  assert {
    condition = (anytrue([for r in aws_network_acl.tb.ingress : r.action == "allow" && contains(["tcp", "6"], r.protocol) && r.cidr_block == "0.0.0.0/0" && r.from_port == 32768 && r.to_port == 60999])
      && anytrue([for r in aws_network_acl.tb.ingress : r.action == "allow" && contains(["icmp", "1"], r.protocol) && r.cidr_block == "0.0.0.0/0" && r.icmp_type == 3 && r.icmp_code == 4])
    && anytrue([for r in aws_network_acl.tb.egress : r.action == "allow" && r.protocol == "-1" && r.cidr_block == "0.0.0.0/0"]))
    error_message = "the network ACL blocks the replies to the instances' HTTPS calls (S3, SSM, dnf) or their path MTU discovery"
  }
  assert {
    condition     = aws_network_acl.tb.tags == tomap({ Name = "topdisc-packed" })
    error_message = "network ACL name changed"
  }
}

run "named" {
  command = apply
  variables {
    deploy = "a5k-21"
  }
  assert {
    condition = (aws_iam_role.host.name == "topdisc-packed-a5k-21-host" && aws_iam_role.coordinator.name == "topdisc-packed-a5k-21-coordinator"
    && aws_iam_instance_profile.host.name == "topdisc-packed-a5k-21-host" && aws_iam_instance_profile.coordinator.name == "topdisc-packed-a5k-21-coordinator")
    error_message = "IAM names lack the id"
  }
  assert {
    condition     = aws_security_group.tb.name == "topdisc-packed-a5k-21" && aws_s3_bucket.tb.bucket_prefix == "topdisc-packed-a5k-21-" && aws_vpc.tb.tags.Name == "topdisc-packed-a5k-21"
    error_message = "SG, bucket or VPC names lack the id"
  }
  assert {
    condition     = aws_instance.host[0].tags.Name == "topdisc-a5k-21-host-0" && aws_instance.host[0].tags.topdisc == "host" && aws_instance.coordinator.tags.Name == "topdisc-a5k-21-coordinator"
    error_message = "instance names lack the id"
  }
  assert {
    condition = alltrue([for t in [aws_vpc.tb.tags, aws_internet_gateway.tb.tags, aws_subnet.hosts.tags, aws_route_table.tb.tags, aws_security_group.tb.tags, aws_s3_bucket.tb.tags,
      aws_iam_role.host.tags, aws_iam_role.coordinator.tags, aws_iam_instance_profile.host.tags, aws_iam_instance_profile.coordinator.tags,
    aws_network_acl.tb.tags, aws_instance.host[0].tags, aws_instance.coordinator.tags] : t["topdisc-deploy"] == "a5k-21"])
    error_message = "a taggable resource lacks topdisc-deploy=a5k-21"
  }
  assert {
    condition     = strcontains(aws_iam_role_policy.coordinator.policy, "ec2:ResourceTag/topdisc-deploy")
    error_message = "the coordinator may terminate other deployments' hosts"
  }
  assert {
    condition     = strcontains(aws_instance.coordinator.user_data, "Name=tag:topdisc-deploy,Values=a5k-21") && strcontains(aws_instance.coordinator.user_data, "/opt/topdisc/deploy")
    error_message = "the coordinator's scale-down or inventory is not scoped to the id"
  }
}

run "bad_id" {
  command = plan
  variables {
    deploy = "Bad_ID"
  }
  expect_failures = [var.deploy]
}

run "default_id" {
  command = plan
  variables {
    deploy = "default"
  }
  expect_failures = [var.deploy]
}

run "long_id" {
  command = plan
  variables {
    deploy = "abcdefghijklmnopqrstu"
  }
  expect_failures = [var.deploy]
}

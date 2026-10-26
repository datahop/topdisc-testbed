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
    aws_instance.host[0].tags, aws_instance.coordinator.tags] : t["topdisc-deploy"] == "a5k-21"])
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

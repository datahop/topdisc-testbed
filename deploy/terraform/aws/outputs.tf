output "binaries_bucket" { value = aws_s3_bucket.binaries.id }
# coordinator_id / coordinator_ip are in regions.tf. The node instances come
# from autoscaling groups, so the inventory is built on the coordinator with
# inventory-aws.sh.

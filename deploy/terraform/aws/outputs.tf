output "coordinator_ip" { value = aws_instance.coordinator.private_ip }
output "binaries_bucket" { value = aws_s3_bucket.binaries.id }
output "coordinator_id" { value = aws_instance.coordinator.id }
output "asg" { value = aws_autoscaling_group.nodes.name }
# The node instances come from an ASG, so the inventory is built on the
# coordinator with ../../inventory-aws.sh once they are up.

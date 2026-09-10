output "coordinator_ip" { value = aws_instance.coordinator.private_ip }
output "asg" { value = aws_autoscaling_group.nodes.name }
# The node instances come from an ASG, so the inventory is built on the
# coordinator with ../../inventory-aws.sh once they are up.

output "bucket" { value = aws_s3_bucket.tb.id }
output "region" { value = var.region }
output "coordinator_id" { value = aws_instance.coordinator.id }
output "coordinator_ip" { value = aws_instance.coordinator.private_ip }

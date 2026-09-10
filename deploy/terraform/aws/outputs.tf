output "coordinator_ip" { value = aws_instance.coordinator.private_ip }
output "host_ips" { value = aws_instance.host[*].private_ip }
output "inventory" {
  description = "consumed by the coordinator's cloud backend"
  value = { coordinator = aws_instance.coordinator.private_ip, hosts = [for i, h in aws_instance.host : { index = i, ip = h.private_ip, nodes = var.nodes_per_host }] }
}

output "coordinator_public_ip" { value = hcloud_server.coordinator.ipv4_address }
output "coordinator_ip" { value = one(hcloud_server.coordinator.network).ip }
output "hosts" {
  value = [for i, h in hcloud_server.host : { index = i, ip = one(h.network).ip, public_ip = h.ipv4_address }]
}

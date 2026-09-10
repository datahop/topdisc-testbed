output "coordinator_public_ip" { value = hcloud_server.coordinator.ipv4_address }
output "inventory" {
  value = { coordinator = one(hcloud_server.coordinator.network).ip, hosts = [for i, h in hcloud_server.host : { index = i, ip = one(h.network).ip, nodes = 1 }] }
}

output "inventory" {
  value = { coordinator = google_compute_instance.coordinator.network_interface[0].network_ip, hosts = [for i, h in google_compute_instance.host : { index = i, ip = h.network_interface[0].network_ip, nodes = var.nodes_per_host }] }
}

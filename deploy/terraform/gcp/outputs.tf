output "coordinator_ip" { value = google_compute_instance.coordinator.network_interface[0].network_ip }
output "mig" { value = google_compute_instance_group_manager.nodes.name }
# Inventory: on the coordinator, ../../inventory-gcp.sh

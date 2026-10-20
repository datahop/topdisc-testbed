output "bucket" { value = google_storage_bucket.tb.name }
output "zone" { value = var.zone }
output "coordinator" { value = google_compute_instance.coordinator.name }
output "coordinator_ip" { value = google_compute_instance.coordinator.network_interface[0].network_ip }

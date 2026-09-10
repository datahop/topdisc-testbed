#!/bin/sh
# Writes inventory.json for the cloud backend from the running fleet.
set -e
gcloud compute instances list --filter='labels.role=host AND status=RUNNING' --format='value(networkInterfaces[0].networkIP)' |
python3 -c '
import json,sys
ips=sorted(l.strip() for l in sys.stdin if l.strip())
print(json.dumps({"coordinator":"","hosts":[{"index":i,"ip":ip,"nodes":1} for i,ip in enumerate(ips)]}))
' > inventory.json
echo "$(python3 -c 'import json;print(len(json.load(open("inventory.json"))["hosts"]))') hosts"

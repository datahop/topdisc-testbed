#!/bin/sh
# Writes inventory.json for the cloud backend from the running fleet.
# Run on the coordinator (instance role has EC2 read access).
set -e
aws ec2 describe-instances --filters Name=tag:role,Values=host Name=instance-state-name,Values=running \
  --query 'Reservations[].Instances[].PrivateIpAddress' --output json |
python3 -c '
import json,sys
ips=sorted(json.load(sys.stdin))
print(json.dumps({"coordinator":"","hosts":[{"index":i,"ip":ip,"nodes":1} for i,ip in enumerate(ips)]}))
' > inventory.json
echo "$(python3 -c 'import json;print(len(json.load(open("inventory.json"))["hosts"]))') hosts"

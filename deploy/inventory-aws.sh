#!/bin/sh
# Writes inventory.json for the cloud backend from the running fleet, across
# the regions listed in /opt/topdisc/regions. Run on the coordinator (its
# instance role has EC2 read access).
set -e
for r in $(cat /opt/topdisc/regions); do
  aws ec2 describe-instances --region "$r" --filters Name=tag:role,Values=host Name=instance-state-name,Values=running \
    --query 'Reservations[].Instances[].PrivateIpAddress' --output text | tr '\t' '\n' | sed "s/^/$r /"
done | python3 -c '
import json,sys
rows=sorted(l.split() for l in sys.stdin if l.strip())
print(json.dumps({"coordinator":"","hosts":[{"index":i,"ip":ip,"nodes":1,"region":r} for i,(r,ip) in enumerate(rows)]}))
' > inventory.json
python3 -c '
import json,collections
h=json.load(open("inventory.json"))["hosts"]; c=collections.Counter(x["region"] for x in h)
print(len(h),"hosts:",dict(c))'

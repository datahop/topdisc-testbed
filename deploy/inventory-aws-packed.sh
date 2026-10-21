#!/bin/sh
# Writes inventory.json for the cloud backend from the running fleet, on the
# coordinator. Host i is the instance topdisc-host-i: the route table sends
# its node /16 to that instance, so the hosts are listed by instance index and
# a gap is an error; routes: network tells the hosts to install no route via
# each other.
#   ./inventory-aws-packed.sh <nodes per host>
set -e
per=${1:?nodes per host}
T=$(curl -s -X PUT http://169.254.169.254/latest/api/token -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
me=$(curl -s -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/local-ipv4)
aws ec2 describe-instances --region "$(cat /opt/topdisc/region)" --filters Name=tag:topdisc,Values=host Name=instance-state-name,Values=running \
  --query 'Reservations[].Instances[].[Tags[?Key==`Name`].Value|[0],PrivateIpAddress]' --output text |
python3 -c '
import json,sys
per=int(sys.argv[1])
hosts=sorted((int(n.rsplit("-",1)[1]),ip) for n,ip in (l.split() for l in sys.stdin if l.strip()))
for want,(i,_) in enumerate(hosts):
    if i!=want: sys.exit("host %d is not running (running: %s)" % (want, [i for i,_ in hosts]))
json.dump({"coordinator":sys.argv[2],"routes":"network","hosts":[{"index":i,"ip":ip,"nodes":per} for i,ip in hosts]},open("inventory.json","w"))
print("%d hosts x %d nodes" % (len(hosts),per))
' "$per" "$me"

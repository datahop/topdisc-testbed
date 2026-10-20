#!/bin/sh
# Writes inventory.json for the cloud backend from the running fleet, on the
# coordinator. Host i is the VM topdisc-host-i: the VPC routes its node /16
# to that VM, so the hosts are listed by VM index and a gap is an error;
# routes: network tells the hosts to install no route via each other.
#   ./inventory-gcp.sh <nodes per host>
set -e
per=${1:?nodes per host}
me=$(curl -s -H Metadata-Flavor:Google http://169.254.169.254/computeMetadata/v1/instance/network-interfaces/0/ip)
gcloud compute instances list --zones "$(cat /opt/topdisc/zone)" --filter='labels.topdisc=host AND name~^topdisc-host-[0-9]+$ AND status=RUNNING' \
  --format='value(name,networkInterfaces[0].networkIP)' |
python3 -c '
import json,sys
per=int(sys.argv[1])
hosts=sorted((int(n.rsplit("-",1)[1]),ip) for n,ip in (l.split() for l in sys.stdin if l.strip()))
for want,(i,_) in enumerate(hosts):
    if i!=want: sys.exit("host %d is not running (running: %s)" % (want, [i for i,_ in hosts]))
json.dump({"coordinator":sys.argv[2],"routes":"network","hosts":[{"index":i,"ip":ip,"nodes":per} for i,ip in hosts]},open("inventory.json","w"))
print("%d hosts x %d nodes" % (len(hosts),per))
' "$per" "$me"

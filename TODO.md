## Bugs
[x] - fix case when prometheus is not available
[ ] - reducing number of replicas should delete old deployments
[ ] - ? increase sync period to avoid races on bulk delete/recreate
[ ] - 
## Features
[ ] - expose metrics about own health and behaviour
[ ] - dynamic scaling up/down based on metrics
[ ] - update GOMAXPROCS based on number of CPUs
[ ] - post behaviour updates to Kubernetes events
## Future
[ ] - call webhooks on scaling events 
[ ] - different sources of lag (promtheus,kafka,http endpoint)
[ ] - 
[ ] - 
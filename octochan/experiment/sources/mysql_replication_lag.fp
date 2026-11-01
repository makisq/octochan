goal: available

classes:
  Replication : "replication"
  Replica     : "replica"
  Latency     : "delay"
  State       : "available"

relations:
  Latency --limits--> Replica (delay_budget)
  Replication --modifies--> Replica (replication_change)
  Replica --strives_to--> State

cell c1:
  set Latency.maximum = 30 s
  set Replication.order = serial

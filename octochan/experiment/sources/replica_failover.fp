goal: available

classes:
  State   : "available"
  Replica : "replica"
  Leader  : "primary"
  Lock    : "lock"

relations:
  Replica --modifies--> Leader (leader_change)
  Lock    --limits-->   Replica (leader_lock)
  Leader  --strives_to--> State

cell c1:
  set Replica.count = 2
  set Leader.timeout = 30 s
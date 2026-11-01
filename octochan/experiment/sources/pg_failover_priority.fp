goal: available

classes:
  Leader  : "primary"
  Replica : "standby"
  Lock    : "lock"
  Timeout : "timeout"
  State   : "available"

relations:
  Timeout --defines--> Leader (leader_timeout)
  Lock --limits--> Leader (leader_lock)
  Leader --strives_to--> State

priority:
  Lock < Timeout

cell c1:
  set Timeout.timeout = 30 s
  set Lock.timeout = 60 s
  set Replica.count = 2

goal: running

classes:
  Restart : "restart"
  Failure : "failure"
  Service : "daemon"
  State   : "running"

relations:
  Failure --defines--> Restart (failure_restart)
  Restart --modifies--> Service (restart_policy)
  Service --strives_to--> State

cell c1:
  set Failure.maximum = 5
  set Restart.delay = 10 s

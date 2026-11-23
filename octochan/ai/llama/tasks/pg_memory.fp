goal: tuned

classes:
  State  : "tuned"
  Config : "config"
  Memory : "memory"
  Limit  : "limit"

relations:
  Memory --limits--> Limit (memory_budget)
  Limit  --modifies--> Config (connection_limit)
  Config --strives_to--> State

priority:
  Memory < Limit

cell c1:
  set Memory.buffers = 8 GB
  set Limit.maximum = 400
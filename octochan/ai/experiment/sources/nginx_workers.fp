goal: tuned

classes:
  Process : "worker"
  Node    : "host"
  Config  : "config"
  State   : "tuned"

relations:
  Node --limits--> Process (host_capacity)
  Process --modifies--> Config (worker_config)
  Config --strives_to--> State

cell c1:
  set Node.capacity = 8
  set Process.maximum = 1024

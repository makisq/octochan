goal: tuned

classes:
  Network    : "network"
  Connection : "connection"
  Memory     : "memory"
  State      : "tuned"

relations:
  Connection --defines--> Network (connection_queue)
  Memory --limits--> Network (memory_budget)
  Network --strives_to--> State

priority:
  Memory < Connection

cell c1:
  set Connection.maximum = 4096
  set Memory.maximum = 1024

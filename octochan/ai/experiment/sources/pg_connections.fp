goal: tuned

classes:
  Connection : "connection"
  Memory     : "memory"
  Config     : "config"
  State      : "tuned"

relations:
  Memory --limits--> Connection (memory_budget)
  Connection --modifies--> Config (session_limit)
  Config --strives_to--> State

priority:
  Memory < Connection

cell c1:
  set Memory.buffer_size = 64 MB
  set Connection.maximum = 300

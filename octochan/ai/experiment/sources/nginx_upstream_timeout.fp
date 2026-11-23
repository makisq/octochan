goal: available

classes:
  Timeout    : "timeout"
  Latency    : "latency"
  Connection : "connection"
  State      : "available"

relations:
  Timeout --defines--> Connection (connection_timeout)
  Latency --limits--> Connection (latency_budget)
  Connection --strives_to--> State

priority:
  Timeout < Latency

cell c1:
  set Timeout.timeout = 60 s
  set Latency.timeout = 5 s

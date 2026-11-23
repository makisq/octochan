goal: tuned

classes:
  Query   : "query"
  Latency : "latency"
  Log     : "log"
  State   : "tuned"

relations:
  Latency --defines--> Log (latency_threshold)
  Log --strives_to--> State

cell c1:
  set Latency.threshold = 2 s
  set Log.threshold = 1..5 s

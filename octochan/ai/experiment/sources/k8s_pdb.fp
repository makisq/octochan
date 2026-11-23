goal: available

classes:
  Count : "number"
  Group : "pool"
  Drain : "drain"
  State : "available"

relations:
  Count --limits--> Drain (count_limit)
  Drain --modifies--> Group (pool_drain)
  Group --strives_to--> State

cell c1:
  set Count.minimum = 2
  set Drain.batch = 1

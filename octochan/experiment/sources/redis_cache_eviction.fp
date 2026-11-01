goal: stable

classes:
  Cache  : "cache"
  Memory : "ram"
  Policy : "policy"
  State  : "stable"

relations:
  Memory --limits--> Cache (memory_cap)
  Policy --defines--> Cache (cache_policy)
  Cache --strives_to--> State

cell c1:
  set Memory.maximum = 4 GB
  set Policy.order = old

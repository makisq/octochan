goal: stable

classes:
  Load    : "workload"
  Limit   : "quota"
  Service : "application"
  State   : "stable"

relations:
  Load --defines--> Service (workload_level)
  Limit --limits--> Service (quota_limit)
  Service --strives_to--> State

cell c1:
  set Load.limit = 2
  set Limit.limit = 1

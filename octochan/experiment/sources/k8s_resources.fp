goal: stable

classes:
  Memory  : "memory"
  Limit   : "quota"
  Service : "application"
  State   : "stable"

relations:
  Memory --defines--> Service (memory_request)
  Limit --limits--> Service (quota_limit)
  Service --strives_to--> State

priority:
  Limit < Memory

cell c1:
  set Memory.limit = 512 MB
  set Limit.limit = 256 MB

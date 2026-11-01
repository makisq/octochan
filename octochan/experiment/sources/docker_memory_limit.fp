goal: stable

classes:
  Memory  : "ram"
  Budget  : "budget"
  Service : "application"
  State   : "stable"

relations:
  Memory --limits--> Service (memory_cap)
  Budget --limits--> Service (budget_cap)
  Service --strives_to--> State

priority:
  Budget < Memory

cell c1:
  set Memory.limit = 2 GB
  set Budget.limit = 1 GB

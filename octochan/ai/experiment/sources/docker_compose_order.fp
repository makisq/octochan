goal: running

classes:
  Order    : "sequence"
  Start    : "start"
  Database : "database"
  State    : "running"

relations:
  Database --precedes--> Start (database_first)
  Order --defines--> Start (start_sequence)
  Start --strives_to--> State

cell c1:
  set Order.order = first
  set Database.timeout = 30 s

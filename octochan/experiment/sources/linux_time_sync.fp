goal: stable

classes:
  Source  : "source"
  Service : "daemon"
  Rate    : "frequency"
  State   : "stable"

relations:
  Source --defines--> Service (service_source)
  Rate --defines--> Service (rate_change)
  Service --strives_to--> State

cell c1:
  set Source.count = 3
  set Rate.period = 64 s

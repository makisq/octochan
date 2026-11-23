goal: healthy

classes:
  Table       : "table"
  Schedule    : "schedule"
  Maintenance : "maintenance"
  State       : "healthy"

relations:
  Schedule --defines--> Maintenance (nightly_window)
  Maintenance --modifies--> Table (table_maintenance)
  Table --strives_to--> State

cell c1:
  set Schedule.period = nightly
  set Maintenance.threshold = 20 %

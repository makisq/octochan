goal: healthy

classes:
  Index       : "index"
  Table       : "table"
  Maintenance : "maintenance"
  State       : "healthy"

relations:
  Maintenance --modifies--> Index (index_maintenance)
  Index --strives_to--> State

cell c1:
  set Maintenance.window = night
  set Table.level = {low, medium}

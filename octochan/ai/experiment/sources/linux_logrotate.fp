goal: available

classes:
  Log      : "log"
  Schedule : "schedule"
  Storage  : "storage"
  State    : "available"

relations:
  Log --modifies--> Storage (log_size)
  Storage --strives_to--> State

cell c1:
  set Log.size = 100 MB
  set Schedule.rate = {daily, weekly}

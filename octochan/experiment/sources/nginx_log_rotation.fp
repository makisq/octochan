goal: available

classes:
  Log      : "log"
  Schedule : "schedule"
  Storage  : "storage"
  State    : "available"

relations:
  Schedule --defines--> Log (log_schedule)
  Log --modifies--> Storage (log_size)
  Storage --strives_to--> State

cell c1:
  set Schedule.rate = daily
  set Log.rate = {daily, weekly}
  set Log.count = 7

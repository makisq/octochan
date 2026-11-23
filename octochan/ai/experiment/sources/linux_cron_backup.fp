goal: safe

classes:
  Task      : "job"
  Schedule  : "schedule"
  Directory : "folder"
  Safety    : "safe"

relations:
  Schedule --defines--> Task (job_schedule)
  Directory --modifies--> Task (folder_source)
  Task --strives_to--> Safety

cell c1:
  set Schedule.rate = nightly
  set Directory.count = 30

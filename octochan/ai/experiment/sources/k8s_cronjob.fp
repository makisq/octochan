goal: running

classes:
  Task     : "job"
  Schedule : "schedule"
  Retry    : "retry"
  State    : "running"

relations:
  Schedule --defines--> Task (job_schedule)
  Retry --limits--> Task (retry_limit)
  Task --strives_to--> State

cell c1:
  set Schedule.rate = nightly
  set Retry.maximum = 3

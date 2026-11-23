goal: healthy

classes:
  Validation : "verification"
  Retry      : "retry"
  Service    : "daemon"
  State      : "healthy"

relations:
  Retry --defines--> Validation (retry_count)
  Validation --modifies--> Service (verification_change)
  Service --strives_to--> State

cell c1:
  set Retry.count = 3
  set Validation.period = 30 s

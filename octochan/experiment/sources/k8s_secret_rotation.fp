goal: safe

classes:
  Secret   : "token"
  Schedule : "schedule"
  Service  : "service"
  Safety   : "safe"

relations:
  Schedule --defines--> Secret (token_schedule)
  Secret --modifies--> Service (token_change)
  Service --strives_to--> Safety

cell c1:
  set Schedule.rate = weekly
  set Secret.rate = {daily, weekly}

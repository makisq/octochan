goal: stable

classes:
  Request : "request"
  Rate    : "rate"
  Limit   : "limit"
  State   : "stable"

relations:
  Rate --limits--> Request (request_rate)
  Limit --defines--> Rate (rate_limit)
  Request --strives_to--> State

cell c1:
  set Limit.size = 20
  set Rate.rate = 100 rps

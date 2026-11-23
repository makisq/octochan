goal: healthy

classes:
  Probe   : "probe"
  Timeout : "timeout"
  Service : "service"
  State   : "healthy"

relations:
  Timeout --defines--> Probe (probe_timeout)
  Probe --modifies--> Service (service_probe)
  Service --strives_to--> State

cell c1:
  set Timeout.timeout = 5 s
  set Probe.period = 10 s

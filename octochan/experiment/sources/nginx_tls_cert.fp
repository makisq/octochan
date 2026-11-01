goal: ready

classes:
  Secret  : "certificate"
  Service : "service"
  Config  : "configuration"
  State   : "ready"

relations:
  Secret --modifies--> Config (certificate_change)
  Config --strives_to--> State

hard forbid write(except = {tls_config})

cell c1:
  set Secret.deadline = 90 d
  set Service.port = 443

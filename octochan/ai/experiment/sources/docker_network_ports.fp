goal: available

classes:
  Port    : "port"
  Address : "address"
  Network : "network"
  State   : "available"

relations:
  Port --modifies--> Network (port_change)
  Address --defines--> Network (network_address)
  Network --strives_to--> State

cell c1:
  set Port.port = 8080
  set Address.size = 24

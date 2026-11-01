goal: safe

classes:
  Network    : "network"
  Port       : "port"
  Permission : "access"
  Safety     : "safe"

relations:
  Port --defines--> Permission (port_access)
  Permission --modifies--> Network (access_change)
  Network --strives_to--> Safety

cell c1:
  set Port.port = 8080
  set Permission.level = high

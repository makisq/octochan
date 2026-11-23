goal: safe

classes:
  Secret     : "key"
  Permission : "access"
  User       : "user"
  Safety     : "safe"

relations:
  Secret --defines--> Permission (key_access)
  Permission --limits--> User (user_access)
  User --strives_to--> Safety

hard forbid write(except = {sshd_config})

cell c1:
  set Secret.state = disabled
  set Permission.maximum = 3

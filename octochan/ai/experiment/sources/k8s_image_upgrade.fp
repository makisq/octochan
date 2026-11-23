goal: release

classes:
  Upgrade     : "upgrade"
  Rollback    : "rollback"
  Version     : "release"
  Environment : "staging"

relations:
  Environment --defines--> Upgrade (staging_first)
  Rollback --terminates--> Upgrade (failure_rollback)
  Upgrade --strives_to--> Version

cell c1:
  set Environment.version = new
  set Upgrade.order = {canary, rolling}

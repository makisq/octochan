goal: release

classes:
  Version    : "release"
  Deployment : "deploy"
  Strategy   : "canary"
  Rollback   : "rollback"

relations:
  Strategy   --precedes--> Deployment (canary_phase)
  Rollback   --terminates--> Deployment (failure_rollback)
  Deployment --strives_to--> Version

cell c1:
  set Strategy.size = 5 %
  set Deployment.window = night
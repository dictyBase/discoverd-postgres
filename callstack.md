## List of function calls that the runner goes through
Command Line
    V
RunCentosPg
    V
startLeader *check* OR promoteToLeader
    V
startFollower *checked* OR switchLeader *check*



__startLeader__

startPostgres *checked*
    V
waitForPostgres *checked*
    V
createSuperuser *checked*

__promoteToLeader__

register *checked*
    V
waitForPromotion *checked*
    V
register *checked*


__startFollower__

waitForLeaderUp   *check*
    V
pullBaseBackup  *check*
    V
writeRecoveryConf *Have to look for folder?*
    V
waitForPostgres *checked*

__switchLeader__

waitforleaderUp *check*
    V
register *check*
    V
writeRecoveryConf *Folder check ?*
    V
startPostgres *check*
    V
waitForPostgres *check*
    V
register *check*


__startPostgres__

writeConfig *Copy file to correct place ?*

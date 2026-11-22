# Security policy

Please don't report security vulnerabilities in public issues.

Report them privately through GitHub: on the repository's **Security** tab,
choose **Report a vulnerability**. Include what you found, how to reproduce
it, and the affected version.

We'll acknowledge the report within a week and keep you updated until it's
fixed. Fixes are released for the latest version only.

## Scope

The node agent runs privileged on every node (it reads host `/proc` and
cgroups), and the controller can change any PVC's VolumeAttributesClass in
namespaces with a policy. Issues that let a user escalate through either are
in scope.

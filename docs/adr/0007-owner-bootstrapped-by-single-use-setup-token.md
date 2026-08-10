# The Owner is bootstrapped by a single-use setup token

The installer prints a short-lived, single-use token. Opening dokkup and redeeming
that token creates the Owner. The token is revoked the moment it is used, and
`dokkup setup-token` reissues one only while no Owner exists.

Letting the first visitor claim ownership through the web would mean a window in
which whoever reaches the port first controls the machine. Creating the account
entirely in the terminal avoids that window but makes the operator type a password
into a shell, where it lands in scrollback and shell history anyway. The token
moves the secret that must survive the terminal from a password to a credential
that expires and can only be spent once.

## Consequences

The reissue command must verify that no Owner exists. Without that condition it
would be an unauthenticated account-takeover path, which is the exact failure the
token was introduced to prevent.

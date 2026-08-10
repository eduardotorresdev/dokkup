# App Link hides Dokku's three network attachment slots

Connecting two Apps is expressed as a link between them. dokkup creates the
network if needed, attaches both Apps, shows the resulting hostname and port, and
tells the operator that the change takes effect on the next rebuild — offering to
perform it.

Dokku exposes three attachment points with distinct semantics: attach at creation,
attach before the container runs, and attach after a successful deploy. Choosing
between them requires knowing which container needs to reach which, and in what
order they start. Passing that choice to the operator would be passing along
precisely the complexity dokkup exists to absorb.

## Consequences

Some legitimate arrangements — services with strict start-up ordering — are not
expressible through App Links. Those remain available through the CLI, and an
advanced view can be added later if real cases appear.

Apps resolve each other at `<app>.<process-type>` and the port is mandatory. That
hostname is surfaced ready to copy, because nobody guesses it.

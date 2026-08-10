// dokkup renders entirely in the browser. There is no Node process on the Dokku
// host to render on, by design.
//
// A consequence worth stating where someone will read it: with no server-side
// rendering there is no server-side route guard. Anything this frontend does to
// hide a page is a user-experience affordance, and authorisation is enforced by
// the API, which answers 401 and 403.
//
// See docs/adr/0004-single-go-binary-with-embedded-csr-frontend.md.
export const ssr = false;
export const prerender = false;
export const trailingSlash = 'never';

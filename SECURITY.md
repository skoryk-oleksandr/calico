# Security Policy

## Supported Versions

The Project Calico community supports the most recent three minor
versions on a rolling basis. Older versions receive security-only
patches at the maintainers' discretion. The current support window is
documented in the release notes for each minor release on
https://docs.tigera.io/calico/latest/release-notes/.

## Reporting a Vulnerability

Please follow responsible disclosure best practices and [Tigera's Vulnerability Disclosure Policy](https://www.tigera.io/vulnerability-disclosure/) when submitting
security vulnerabilities.  **Do not** create a GitHub issue or pull 
request because those are immediately public. Instead:

*  Email [psirt@projectcalico.org](psirt@projectcalico.org).
*  Report a private [security advisory](https://github.com/projectcalico/calico/security/advisories)
  through the GitHub interface.

Please include as much information as possible, including the
affected version(s) and steps to reproduce.

## Bug Bounty

Project Calico does not currently operate a bug bounty programme. Researchers who would like to be acknowledged for valid reports can request a public mention in the release notes for the version that ships the fix.

## Third Party Vulnerabilities

When using automated security scanning tools (e.g., Trivy, Grype, Docker Scout), CVEs may be flagged in Calico container images due to vulnerabilities in third-party dependencies. Before submitting any reports related to these findings, check the [Tigera VEX repository](https://github.com/tigera/vex).
The repository provides analysis of third-party CVEs that may appear in Calico images, including whether they are exploitable or applicable to our supported versions. Reviewing this information helps avoid duplicate reports and offers context for scanner-detected issues.
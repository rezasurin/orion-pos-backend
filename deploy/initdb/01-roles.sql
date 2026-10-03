-- Local only: the login roles the service and operators connect as. Production creates these
-- by hand or with infrastructure code (see deploy/README.md).
CREATE ROLE orion_app NOLOGIN;
CREATE ROLE orion_platform NOLOGIN;

CREATE ROLE orion_api LOGIN PASSWORD 'orion' IN ROLE orion_app;
CREATE ROLE orion_admin LOGIN PASSWORD 'orion' IN ROLE orion_platform;

ALTER TABLE operation_lease
    ADD CONSTRAINT operation_lease_resource_type_check
    CHECK (resource_type IN ('sandbox', 'conversation'));

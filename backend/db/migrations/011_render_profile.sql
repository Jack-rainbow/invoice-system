-- A render profile is deployment configuration and its content is immutable.
-- The profile below records only what this deployment actually knows: paper,
-- locale, timezone, the fixed calculation version and the typography floors.
-- Template/font bytes and the renderer protocol are deliberately null, so no
-- preview or formal render can claim an asset that has not been provisioned.
INSERT INTO render_profiles(id,name,manifest,manifest_hash,status) VALUES(
 '00000000-0000-4000-8000-000000000001',
 'mxn-iva16-draft-v1',
 '{"schema_version":"render-profile/1","calculation_version":"mxn-iva16-halfup-v1","locale":"es-MX","timezone":"America/Mexico_City","paper":{"width_pt":"569.52","height_pt":"737.28","logical_width":1582,"logical_height":2048,"pt_per_pixel":"0.36"},"typography":{"min_primary_pt":8,"min_secondary_pt":6},"assets":null,"renderer_protocol":null}'::jsonb,
 'ebea36d39d828c30b90fc8b82519656b72833dea19f9646679e49daf0721c22d',
 'ACTIVE');

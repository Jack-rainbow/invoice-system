-- Template v3: single-line fields autofit (bounded font shrink) instead of
-- refusing, matching the reference tool; the floor stays at 17px (~6.1pt),
-- never below the manifest's secondary minimum.
UPDATE render_profiles SET status='REVOKED', updated_at=now()
WHERE id='00000000-0000-4000-8000-000000000003' AND status='ACTIVE';
INSERT INTO render_profiles(id,name,manifest,manifest_hash,status) VALUES(
 '00000000-0000-4000-8000-000000000004',
 'mxn-iva16-cfdi-raster-v3',
 '{"assets":{"bundle":"invoice-mxn-v1","sha256":{"NotoSans-Bold.ttf":"b3788af7d1d7d09f6227abb1c50185f95fa5b908f6a39343cf3a5913b656eb16","NotoSans-Regular.ttf":"b87e786b03ee28e3d014cbc717a3b3848abf4f650dada84b5a69ee21e48c3d2e","WatermarkCJK.ttf":"d011802f394bef9eb22ffd56a93a37d970e66860c10bf80a5f18fdc5f2ad46e7","template.json":"153afee275ca3c6f50a096a82b9874105ab59d7a58ec56be96d73c581c3512c5"}},"calculation_version":"mxn-iva16-halfup-v1","locale":"es-MX","paper":{"height_pt":"737.28","logical_height":2048,"logical_width":1582,"pt_per_pixel":"0.36","width_pt":"569.52"},"renderer_protocol":"goimage-raster/1","schema_version":"render-profile/1","timezone":"America/Mexico_City","typography":{"autofit_min_px":17,"min_primary_pt":8,"min_secondary_pt":6},"watermark":{"alpha":0.32,"angle_degrees":30,"max_bytes":2097152,"style_version":"preview-watermark/1","text":"预览 未正式生成","width_px":1000}}'::jsonb,
 '36be98958bfcc577648bea11ac630e6f0845aa3aa38388b9f2cc51b03216fded',
 'ACTIVE');

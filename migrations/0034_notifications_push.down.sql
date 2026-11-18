-- Reverts 0034_notifications_push.
--
-- The API and worker release that needs this migration must be rolled back
-- first. LOSES: every registered device, in-app notification, push / email
-- delivery record and notification preference (with its change history).

DROP TABLE IF EXISTS notification_preference_changes;
DROP TABLE IF EXISTS notification_preferences;
DROP TABLE IF EXISTS notification_deliveries;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS user_devices;

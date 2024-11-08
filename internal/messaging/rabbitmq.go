package messaging

import (
	"encoding/json"
	"fmt"
	"log"
	"net/smtp"
	"typeMore/utils"

	"github.com/streadway/amqp"
)

type RabbitMQConfig struct {
	URL string
}

type RabbitMQConnection struct {
	conn    *amqp.Connection
	channel *amqp.Channel
}
type EmailMessage struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}


func ConnectRabbitMQ(cfg RabbitMQConfig) (*RabbitMQConnection, error) {

	conn, err := amqp.Dial(cfg.URL)
	if err != nil {
		return nil, err
	}


	channel, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, err
	}

	return &RabbitMQConnection{conn: conn, channel: channel}, nil
}


func (r *RabbitMQConnection) Close() {
	if r.channel != nil {
		r.channel.Close()
	}
	if r.conn != nil {
		r.conn.Close()
	}
}

func (r *RabbitMQConnection) PublishMessage(queueName string, emailMessage EmailMessage) error {

	message, err := json.Marshal(emailMessage)
	if err != nil {
		return err
	}


	_, err = r.channel.QueueDeclare(
		queueName,
		true,  // Durable
		false, // Delete when unused
		false, // Exclusive
		false, // No-wait
		nil,   // Arguments
	)
	if err != nil {
		return err
	}
	err = r.channel.Publish(
		"",         // Exchange
		queueName,  // Routing key
		false,      // Mandatory
		false,      // Immediate
		amqp.Publishing{
			ContentType: "application/json",
			Body:        message,
		})
	return err
}

func SendEmail(email EmailMessage) error {

	smtpHost := utils.GetEnv("SMTP_HOST","")
	smtpPort := utils.GetEnv("SMTP_PORT","")
	from := utils.GetEnv("EMAIL_USERNAME","")
	password := utils.GetEnv("EMAIL_PASSWORD","")

	auth := smtp.PlainAuth("", from, password, smtpHost)

	to := []string{email.To}
	msg := []byte(fmt.Sprintf("Subject: %s\r\n\r\n%s", email.Subject, email.Body))


	err := smtp.SendMail(smtpHost+":"+smtpPort, auth, from, to, msg)
	if err != nil {
		return fmt.Errorf("failed to send email: %w", err)
	}
	return nil
}

func (r *RabbitMQConnection) ConsumeMessages(queueName string) error {
	msgs, err := r.channel.Consume(
		queueName,
		"",    // Consumer
		true,  // Auto-acknowledge
		false, // Exclusive
		false, // No-local
		false, // No-wait
		nil,   // Arguments
	)
	if err != nil {
		return err
	}


	for msg := range msgs {
		var email EmailMessage
		if err := json.Unmarshal(msg.Body, &email); err != nil {
			log.Printf("Error unmarshalling email message: %v", err)
			continue
		}

	
		if err := SendEmail(EmailMessage{
			To:      email.To,
			Subject: email.Subject,
			Body:    email.Body,
		}); err != nil {
			log.Printf("Error sending email: %v", err)
		} else {
			log.Printf("Email sent to: %s", email.To)
		}
	}

	return nil
}
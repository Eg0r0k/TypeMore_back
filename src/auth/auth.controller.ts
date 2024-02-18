import { Controller, Post, Body, Req, Res, Logger } from '@nestjs/common';
import { AuthService } from './auth.service';
import { Request, Response } from 'express';
import { RegisterUserDto } from './dto/register-user.dto';
import { LoginDto } from './dto/login-user.dto';

@Controller('auth')
export class AuthController {
  private readonly logger = new Logger(AuthController.name);
  constructor(private readonly authService: AuthService) {}
  @Post('login')
  async login(
    @Req() request: Request,
    @Res() response: Response,
    @Body() loginDto: LoginDto
  ): Promise<any> {
    try {
      const maxAgeCookieToken = 24 * 60 * 60 * 1000;
      const result = await this.authService.login(loginDto);
      response.cookie('jwt', result, {
        httpOnly: true,
        maxAge: maxAgeCookieToken,
        secure: process.env.NODE_ENV === 'production'
      });
      return response.status(200).json({
        status: 'OK',
        message: 'Successfully login!',
        result: result
      });
    } catch (error) {
      return response.status(500).json({
        status: 'Error',
        message: 'Server Error!'
      });
    }
  }
  @Post('/register')
  async register(
    @Req() request: Request,
    @Res() response: Response,
    @Body() registerDto: RegisterUserDto
  ): Promise<any> {
    try {
      const result = await this.authService.register(registerDto);
      this.logger.log(`User successfully registred`);
      return response.status(200).json({
        status: 'OK',
        message: 'Successfully register user!',
        result: result
      });
    } catch (error: any) {
      this.logger.error(`Registration error: ${error.message}`, error.stack);
      return response.status(500).json({
        status: 'Error',
        message: error.message
      });
    }
  }
}
